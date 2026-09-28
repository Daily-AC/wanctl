package relay

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// registerFrom registers a client the way a connector does, arriving through
// the reverse proxy on behalf of clientAddr.
func registerFrom(t *testing.T, r *Relay, clientAddr string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(
		`{"client_name":"Connector","redirect_uris":["`+testRedirect+`"],"token_endpoint_auth_method":"client_secret_post"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("X-Real-IP", clientAddr)
	return oauthDo(t, r, req)
}

func wantTemporarilyUnavailable(t *testing.T, rr *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rr.Code != status {
		t.Fatalf("status = %d, want %d: %s", rr.Code, status, rr.Body.String())
	}
	out := decode(t, rr)
	if out["error"] != "temporarily_unavailable" || out["error_description"] == "" {
		t.Fatalf("body = %v, want an RFC 7591 error object", out)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("refusal carries no Retry-After")
	}
}

// Registration needs no account, and every one is a row in the database. One
// client gets a handful an hour; the next client is unaffected.
func TestClientRegistrationIsBudgetedPerClient(t *testing.T) {
	r, _, _ := newOAuthRelay(t)
	for i := 0; i < oauthRegistrationsPerClient; i++ {
		if rr := registerFrom(t, r, "198.51.100.1"); rr.Code != http.StatusCreated {
			t.Fatalf("registration %d = %d %s", i+1, rr.Code, rr.Body.String())
		}
	}
	wantTemporarilyUnavailable(t, registerFrom(t, r, "198.51.100.1"), http.StatusTooManyRequests)
	if rr := registerFrom(t, r, "198.51.100.2"); rr.Code != http.StatusCreated {
		t.Fatalf("another client = %d, want 201: %s", rr.Code, rr.Body.String())
	}
}

// However many clients ask, the table of clients nobody has used is bounded.
// A client that completed an authorization no longer counts: it was approved
// by a signed-in human, not merely registered.
func TestClientRegistrationHasACeiling(t *testing.T) {
	r, b, _ := newOAuthRelay(t)
	now := time.Now()
	for i := 0; i < oauthMaxUnusedClients; i++ {
		id := fmt.Sprintf("wco_prefilled_%03d", i)
		b.clients[id] = OAuthClient{ID: id, Name: "x", RedirectURIs: []string{testRedirect}, AuthMethod: "none", CreatedAt: now}
	}
	wantTemporarilyUnavailable(t, registerFrom(t, r, "198.51.100.3"), http.StatusServiceUnavailable)

	b.refresh["used"] = OAuthRefresh{Hash: "used", ClientID: "wco_prefilled_000", Namespace: "alice", ExpiresAt: now.Add(time.Hour)}
	if rr := registerFrom(t, r, "198.51.100.3"); rr.Code != http.StatusCreated {
		t.Fatalf("with one client used, registration = %d, want 201: %s", rr.Code, rr.Body.String())
	}
}

// A client registered more than a day ago that never completed an
// authorization is forgotten the next time anyone registers — unless its
// authorization is under way right now, which must still finish.
func TestStaleUnusedClientsAreForgotten(t *testing.T) {
	r, b, _ := newOAuthRelay(t)
	old := time.Now().Add(-oauthUnusedClientTTL - time.Hour)
	b.clients["wco_stale"] = OAuthClient{ID: "wco_stale", Name: "x", RedirectURIs: []string{testRedirect}, AuthMethod: "none", CreatedAt: old}
	b.clients["wco_used"] = OAuthClient{ID: "wco_used", Name: "x", RedirectURIs: []string{testRedirect}, AuthMethod: "none", CreatedAt: old}
	b.refresh["used"] = OAuthRefresh{Hash: "used", ClientID: "wco_used", Namespace: "alice", ExpiresAt: time.Now().Add(time.Hour)}
	b.clients["wco_recent"] = OAuthClient{ID: "wco_recent", Name: "x", RedirectURIs: []string{testRedirect}, AuthMethod: "none", CreatedAt: time.Now().Add(-time.Hour)}

	// An old client whose owner is on the consent page right now.
	id, secret := registerClient(t, r)
	stored := b.clients[id]
	stored.CreatedAt = old
	b.clients[id] = stored
	verifier, challenge := pkce()
	code := authorize(t, r, id, challenge, "st", "alice")

	if rr := registerFrom(t, r, "198.51.100.4"); rr.Code != http.StatusCreated {
		t.Fatalf("registration = %d %s", rr.Code, rr.Body.String())
	}
	if _, ok := b.clients["wco_stale"]; ok {
		t.Error("a day-old client that never authorized was kept")
	}
	for _, keep := range []string{"wco_used", "wco_recent", id} {
		if _, ok := b.clients[keep]; !ok {
			t.Errorf("client %s was forgotten", keep)
		}
	}
	rr := formPost(t, r, "/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {testRedirect},
		"code_verifier": {verifier}, "client_id": {id}, "client_secret": {secret},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("the authorization under way did not complete: %d %s", rr.Code, rr.Body.String())
	}
}

// The same rules against a real database, including the parts a map cannot
// get wrong: the anti-join that defines "never used", and an in-flight list
// that is empty or absent.
func TestOAuthPostgresRegistrationForgetsStaleClientsAndCaps(t *testing.T) {
	p, db, exec := pgDeviceIDStore(t)
	now := time.Now()
	old := now.Add(-oauthUnusedClientTTL - time.Hour)
	client := func(id string, created time.Time) OAuthClient {
		return OAuthClient{ID: id, Name: "x", RedirectURIs: []string{testRedirect}, AuthMethod: "none", CreatedAt: created}
	}
	register := func(c OAuthClient, maxUnused int, staleBefore time.Time, inFlight []string) bool {
		t.Helper()
		ok, err := p.RegisterOAuthClient(c, maxUnused, staleBefore, inFlight)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	has := func(id string) bool {
		t.Helper()
		_, found, err := p.OAuthClient(id)
		if err != nil {
			t.Fatal(err)
		}
		return found
	}
	for _, c := range []OAuthClient{client("wco_stale", old), client("wco_used", old), client("wco_inflight", old), client("wco_recent", now.Add(-time.Hour))} {
		if !register(c, 10, time.Time{}, nil) {
			t.Fatalf("seeding %s was refused", c.ID)
		}
	}
	exec(`INSERT INTO oauth_refresh_tokens (hash, client_id, namespace, grant_envelope, expires_at)
	      VALUES ('h', 'wco_used', 'alice', 'g', now() + interval '1 hour')`)

	if !register(client("wco_new", now), 10, now.Add(-oauthUnusedClientTTL), []string{"wco_inflight"}) {
		t.Fatal("registration refused under the ceiling")
	}
	if has("wco_stale") {
		t.Error("a stale client that never authorized was kept")
	}
	for _, id := range []string{"wco_used", "wco_inflight", "wco_recent", "wco_new"} {
		if !has(id) {
			t.Errorf("%s was deleted", id)
		}
	}

	// Unused now: wco_inflight, wco_recent, wco_new. The used client is not
	// counted, so a ceiling of three is full and one of four is not.
	if register(client("wco_over", now), 3, now.Add(-oauthUnusedClientTTL), []string{"wco_inflight"}) || has("wco_over") {
		t.Fatal("a registration past the ceiling was stored")
	}
	if !register(client("wco_fits", now), 4, now.Add(-oauthUnusedClientTTL), []string{"wco_inflight"}) {
		t.Fatal("the used client was counted against the ceiling")
	}

	// With nothing in flight — nil, as a relay with no pending consent passes —
	// the stale in-flight client is now just stale.
	if !register(client("wco_later", now), 10, now.Add(-oauthUnusedClientTTL), nil) {
		t.Fatal("registration refused")
	}
	if has("wco_inflight") {
		t.Error("a stale client was kept when no authorization was in flight")
	}
	var total int
	if err := db.QueryRow(`SELECT count(*) FROM oauth_clients`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 5 { // used, recent, new, fits, later
		t.Errorf("%d clients remain, want 5", total)
	}
}

// The budget refills when its window ends, and it tracks a bounded number of
// addresses: past that, a newcomer waits rather than growing the map.
func TestRegistrationBudgetRefillsAndStaysBounded(t *testing.T) {
	var b registrationBudget
	start := time.Now()
	for i := 0; i < oauthRegistrationsPerClient; i++ {
		if ok, _ := b.take("a", start); !ok {
			t.Fatalf("registration %d refused", i+1)
		}
	}
	ok, wait := b.take("a", start.Add(time.Minute))
	if ok || wait != oauthRegistrationWindow-time.Minute {
		t.Fatalf("spent budget = %v, wait %v; want refused until the window ends", ok, wait)
	}
	if ok, _ := b.take("a", start.Add(oauthRegistrationWindow)); !ok {
		t.Fatal("the budget did not refill after its window")
	}

	for i := len(b.windows); i < oauthRegistrationClients; i++ {
		b.take(fmt.Sprintf("client-%d", i), start.Add(oauthRegistrationWindow))
	}
	if ok, _ := b.take("newcomer", start.Add(oauthRegistrationWindow)); ok || len(b.windows) != oauthRegistrationClients {
		t.Fatalf("a full map admitted a newcomer: ok=%v, %d windows", ok, len(b.windows))
	}
	if ok, _ := b.take("newcomer", start.Add(2*oauthRegistrationWindow)); !ok || len(b.windows) != 1 {
		t.Fatalf("expired windows were not reclaimed: ok=%v, %d windows", ok, len(b.windows))
	}
}
