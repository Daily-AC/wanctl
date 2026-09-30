package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const disableTestSecret = "0123456789abcdef0123456789abcdef-disable"

// disableTestStore is a migrated schema with two accounts, alice and bob, each
// holding one access token.
func disableTestStore(t *testing.T) (p *PGStore, alice, bob string) {
	t.Helper()
	db := deviceIDTestDB(t)
	if err := runMigrations(db, migrationFiles); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users (provider, provider_subject, namespace, name, role) VALUES
 ('github','101','alice','Alice','user'), ('github','102','bob','Bob','user')`); err != nil {
		t.Fatal(err)
	}
	p = &PGStore{db: db}
	var err error
	if alice, err = p.IssueToken("alice", "cli", 0); err != nil {
		t.Fatal(err)
	}
	if bob, err = p.IssueToken("bob", "cli", 0); err != nil {
		t.Fatal(err)
	}
	return p, alice, bob
}

// Disabling is a check where credentials are resolved, not a revocation: while
// the flag is set nothing of bob's resolves and he cannot sign in, alice is
// untouched, and clearing it gives bob back the very same token.
func TestDisabledAccountResolvesNothingUntilEnabled(t *testing.T) {
	p, alice, bob := disableTestStore(t)
	resolves := func(token, want string) bool {
		ns, ok := p.Resolve(token)
		a, okAccess := p.ResolveAccess(token)
		if ok != okAccess || (ok && (ns != want || a.Namespace != want)) {
			t.Fatalf("Resolve and ResolveAccess disagree for %s: %q %v / %q %v", want, ns, ok, a.Namespace, okAccess)
		}
		return ok
	}
	if !resolves(bob, "bob") || !resolves(alice, "alice") {
		t.Fatal("fresh tokens do not resolve")
	}
	if at, found, err := p.SetAccountDisabled("bob", true); err != nil || !found || at == nil {
		t.Fatalf("disable bob: at=%v found=%v err=%v", at, found, err)
	}
	if resolves(bob, "bob") {
		t.Fatal("a disabled account's token still resolves")
	}
	if !resolves(alice, "alice") {
		t.Fatal("disabling bob cut alice off")
	}
	if live, err := p.ExtendRelayTokenHash("bob", HashToken(bob), time.Now().Add(time.Hour)); err != nil || live {
		t.Fatalf("a disabled account's OAuth grant refreshed (live=%v err=%v)", live, err)
	}
	if _, _, err := p.ResolveIdentity("github", "102", "bob", "Bob", "portal"); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("a disabled account signed in: %v", err)
	}
	if ns, _, err := p.ResolveIdentity("github", "101", "alice", "Alice", "portal"); err != nil || ns != "alice" {
		t.Fatalf("alice could not sign in: %q %v", ns, err)
	}

	if at, found, err := p.SetAccountDisabled("bob", false); err != nil || !found || at != nil {
		t.Fatalf("enable bob: at=%v found=%v err=%v", at, found, err)
	}
	if !resolves(bob, "bob") {
		t.Fatal("bob's token did not come back with his account")
	}
	if ns, _, err := p.ResolveIdentity("github", "102", "bob", "Bob", "portal"); err != nil || ns != "bob" {
		t.Fatalf("bob could not sign in again: %q %v", ns, err)
	}
	if _, found, err := p.AccountDisabled("nobody"); err != nil || found {
		t.Fatalf("an unknown namespace was found (%v, %v)", found, err)
	}
}

// The operator's switch cuts bob off everywhere at once: his live session is
// ended and his device is dropped from the registry and cannot poll back in,
// while alice's session carries on. Enabling lets the same device back.
func TestAdminDisableEndsTheAccountsSessions(t *testing.T) {
	p, alice, bob := disableTestStore(t)
	r := New(p)
	r.SetAdmin(p)
	r.SetAdminSecret(disableTestSecret)
	r.SetPortalNS("portal")
	t.Cleanup(func() { closeAllHTTPSessions(r) })
	registerHTTPAgent(t, r, alice, "alice/home-pc")
	registerHTTPAgent(t, r, bob, "bob/laptop")
	codeA, sidA := dialAs(t, r, alice, "alice/home-pc")
	codeB, sidB := dialAs(t, r, bob, "bob/laptop")
	if codeA != http.StatusOK || codeB != http.StatusOK {
		t.Fatalf("dials answered %d (%s) and %d (%s)", codeA, sidA, codeB, sidB)
	}

	admin := func(method string, body any) (int, map[string]any) {
		var buf bytes.Buffer
		if body != nil {
			json.NewEncoder(&buf).Encode(body)
		}
		req := httptest.NewRequest(method, "/admin/users/disable?namespace=bob", &buf)
		req.Header.Set("X-Admin-Secret", disableTestSecret)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.Handler().ServeHTTP(rec, req)
		var out map[string]any
		json.NewDecoder(rec.Body).Decode(&out)
		return rec.Code, out
	}
	if code, out := admin(http.MethodPost, map[string]any{"namespace": "bob", "disabled": true}); code != http.StatusOK || out["disabled"] != true {
		t.Fatalf("disable answered %d %v", code, out)
	}
	if r.session(sidB) != nil {
		t.Fatal("bob's session outlived his account")
	}
	if r.session(sidA) == nil {
		t.Fatal("disabling bob ended alice's session")
	}
	if r.deviceLive("bob", "laptop") {
		t.Fatal("bob's device is still live")
	}
	pollAs := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/h/poll?device=laptop&inst=one", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		ctx, cancel := contextCancelled()
		defer cancel()
		rec := httptest.NewRecorder()
		r.Handler().ServeHTTP(rec, req.WithContext(ctx))
		return rec.Code
	}
	// Refused, but not with the 401 an agent gives up on: it keeps polling and
	// is back on its own once bob is enabled.
	if code := pollAs(bob); code != http.StatusForbidden {
		t.Fatalf("bob's device polled back in, or was told to give up, with %d", code)
	}
	if code := pollAs("not-a-token"); code != http.StatusUnauthorized {
		t.Fatalf("an unknown token answered %d, want 401", code)
	}
	if code, out := admin(http.MethodGet, nil); code != http.StatusOK || out["disabled"] != true {
		t.Fatalf("status answered %d %v", code, out)
	}
	if code, out := admin(http.MethodPost, map[string]any{"namespace": "bob", "disabled": false}); code != http.StatusOK || out["disabled"] != false {
		t.Fatalf("enable answered %d %v", code, out)
	}
	if code := pollAs(bob); code != http.StatusOK || !r.deviceLive("bob", "laptop") {
		t.Fatalf("bob's device could not come back (%d)", code)
	}

	// The portal's own namespace is never disabled, and nobody is disabled
	// without the secret.
	req := httptest.NewRequest(http.MethodPost, "/admin/users/disable", bytes.NewBufferString(`{"namespace":"portal","disabled":true}`))
	req.Header.Set("X-Admin-Secret", disableTestSecret)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("disabling the portal namespace answered %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/admin/users/disable", bytes.NewBufferString(`{"namespace":"bob","disabled":true}`))
	rec = httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("disable without the secret answered %d", rec.Code)
	}
}

// contextCancelled makes a poll register and return instead of parking.
func contextCancelled() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx, cancel
}
