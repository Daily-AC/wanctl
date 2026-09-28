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
	for i := 0; i < 10; i++ {
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
	for i := 0; i < 500; i++ {
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
	old := time.Now().Add(-25 * time.Hour)
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
