package relay

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"wanctl/internal/mcpauth"
)

// The namespace token an authorization mints is what every access token
// carries, so it is the credential that actually opens the user's devices. It
// lives exactly as long as the refresh token that can renew it, and each
// renewal carries it forward: a connector in use never notices, and one that
// stops refreshing leaves nothing behind that works forever.
func TestOAuthNamespaceTokenLivesAsLongAsItsRefreshChain(t *testing.T) {
	r, b, seed := newOAuthRelay(t)
	id, secret := registerClient(t, r)
	verifier, challenge := pkce()
	code := authorize(t, r, id, challenge, "st", "alice")
	rr := formPost(t, r, "/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {testRedirect},
		"code_verifier": {verifier}, "client_id": {id}, "client_secret": {secret},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("code exchange: %d %s", rr.Code, rr.Body.String())
	}
	first := decode(t, rr)
	claim, err := mcpauth.OpenAccess(seed, first["access_token"].(string), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	expiry := b.expiry(claim.Token)
	if expiry.IsZero() {
		t.Fatal("the namespace token minted for the grant never expires")
	}
	if want := time.Now().Add(oauthRefreshTTL); expiry.Before(want.Add(-time.Minute)) || expiry.After(want.Add(time.Minute)) {
		t.Fatalf("namespace token expires at %v, want with its refresh token around %v", expiry, want)
	}

	// Later: the token has an hour left when the connector refreshes. The
	// renewal carries it forward with the new refresh token.
	b.setExpiry(claim.Token, time.Now().Add(time.Hour))
	rr = formPost(t, r, "/oauth/token", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {first["refresh_token"].(string)},
		"client_id": {id}, "client_secret": {secret},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", rr.Code, rr.Body.String())
	}
	second := decode(t, rr)
	if got := b.expiry(claim.Token); got.Before(time.Now().Add(oauthRefreshTTL - time.Minute)) {
		t.Fatalf("after a refresh the namespace token expires at %v, want about %v from now", got, oauthRefreshTTL)
	}
	if _, ok := b.Resolve(claim.Token); !ok {
		t.Fatal("the renewed namespace token does not resolve")
	}

	// A token that is gone ends the chain: the refresh is refused rather than
	// answered with access tokens that fail on their first use.
	b.setExpiry(claim.Token, time.Now().Add(-time.Minute))
	rr = formPost(t, r, "/oauth/token", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {second["refresh_token"].(string)},
		"client_id": {id}, "client_secret": {secret},
	})
	if rr.Code != http.StatusBadRequest || decode(t, rr)["error"] != "invalid_grant" {
		t.Fatalf("refresh on an expired namespace token: %d %s", rr.Code, rr.Body.String())
	}
	if _, ok := b.Resolve(claim.Token); ok {
		t.Fatal("a refused refresh brought the expired namespace token back")
	}
}

// The Postgres half of the rule, against a real database: renewal moves a live
// token's expiry, gives one minted before the rule an expiry, and leaves a
// revoked or lapsed token — or someone else's — exactly as it was.
func TestOAuthPostgresRenewalMovesOnlyALiveToken(t *testing.T) {
	p, db, exec := pgDeviceIDStore(t)
	expiresAt := func(token string) (time.Time, bool) {
		t.Helper()
		var at *time.Time
		if err := db.QueryRow(`SELECT expires_at FROM tokens WHERE hash = $1`, HashToken(token)).Scan(&at); err != nil {
			t.Fatal(err)
		}
		if at == nil {
			return time.Time{}, false
		}
		return *at, true
	}
	until := time.Now().Add(40 * 24 * time.Hour).Truncate(time.Second)

	live, err := p.IssueToken("alice", "oauth:test", oauthTokenDays)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := p.ExtendRelayTokenHash("bob", HashToken(live), until); err != nil || ok {
		t.Fatalf("renewal through another namespace = %v, %v", ok, err)
	}
	if ok, err := p.ExtendRelayTokenHash("alice", HashToken(live), until); err != nil || !ok {
		t.Fatalf("renewing a live token = %v, %v", ok, err)
	}
	if at, _ := expiresAt(live); !at.Equal(until) {
		t.Fatalf("expires_at = %v, want %v", at, until)
	}

	forever, err := p.IssueToken("alice", "oauth:before-the-rule", 0)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := p.ExtendRelayTokenHash("alice", HashToken(forever), until); err != nil || !ok {
		t.Fatalf("renewing a token with no expiry = %v, %v", ok, err)
	}
	if _, has := expiresAt(forever); !has {
		t.Fatal("a renewed token still never expires")
	}

	if err := p.RevokeRelayTokenHash("alice", HashToken(live)); err != nil {
		t.Fatal(err)
	}
	if ok, err := p.ExtendRelayTokenHash("alice", HashToken(live), until.Add(time.Hour)); err != nil || ok {
		t.Fatalf("renewing a revoked token = %v, %v", ok, err)
	}
	if _, ok := p.Resolve(live); ok {
		t.Fatal("a revoked token resolves after a renewal attempt")
	}

	lapsed, err := p.IssueToken("alice", "oauth:lapsed", oauthTokenDays)
	if err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE tokens SET expires_at = now() - interval '1 minute' WHERE hash = $1`, HashToken(lapsed))
	if ok, err := p.ExtendRelayTokenHash("alice", HashToken(lapsed), until); err != nil || ok {
		t.Fatalf("renewing a lapsed token = %v, %v", ok, err)
	}
	if _, ok := p.Resolve(lapsed); ok {
		t.Fatal("a lapsed token resolves after a renewal attempt")
	}
}
