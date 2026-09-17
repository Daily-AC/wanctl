package portal

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const testConsentRedirect = "https://chatgpt.com/connector_platform_oauth_redirect"

// oauthPortal fakes the relay's three internal OAuth endpoints. seen records
// what the portal forwarded, which is the thing most worth asserting: the
// portal must pass the client's request through unchanged rather than
// re-interpreting parameters the relay is about to validate.
func oauthPortal(t *testing.T, seen map[string]any, requestStatus int) *Server {
	t.Helper()
	return newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "alice", "role": "user"})
		case "/admin/oauth/authorize-request":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			seen["authorize-request"] = body
			if requestStatus != http.StatusOK {
				w.WriteHeader(requestStatus)
				json.NewEncoder(w).Encode(map[string]string{
					"error": "invalid_client", "error_description": "unknown client_id; register first"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"request_id": "req-1", "client_name": "ChatGPT", "redirect_uri": testConsentRedirect})
		case "/admin/oauth/approve":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			seen["approve"] = body
			json.NewEncoder(w).Encode(map[string]any{"redirect": testConsentRedirect + "?code=abc&state=st"})
		case "/admin/oauth/deny":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			seen["deny"] = body
			json.NewEncoder(w).Encode(map[string]any{"redirect": testConsentRedirect + "?error=access_denied"})
		default:
			t.Errorf("unexpected relay call: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func authorizeURL() string {
	q := url.Values{
		"client_id": {"wco_1"}, "redirect_uri": {testConsentRedirect}, "response_type": {"code"},
		"code_challenge": {strings.Repeat("c", 43)}, "code_challenge_method": {"S256"},
		"state": {"st"}, "scope": {"wanctl"}, "resource": {"https://relay.test/mcp"},
	}
	return "https://portal.test/oauth/authorize?" + q.Encode()
}

func TestConsentPageNamesTheClientAndItsDestination(t *testing.T) {
	seen := map[string]any{}
	s := oauthPortal(t, seen, http.StatusOK)
	req := httptest.NewRequest(http.MethodGet, authorizeURL(), nil)
	req.Header.Set("X-User", "alice@example.com")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"ChatGPT", "alice", "chatgpt.com", `data-request="req-1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("consent page does not show %q", want)
		}
	}
	// The page is a decision, not the decision: nothing may be granted by the
	// GET that renders it.
	if _, granted := seen["approve"]; granted {
		t.Error("rendering the consent page approved the request")
	}
	forwarded, _ := seen["authorize-request"].(map[string]any)
	if forwarded["client_id"] != "wco_1" || forwarded["code_challenge"] != strings.Repeat("c", 43) ||
		forwarded["state"] != "st" || forwarded["resource"] != "https://relay.test/mcp" {
		t.Errorf("the portal did not forward the request unchanged: %v", forwarded)
	}
}

// The client's whole request has to survive the GitHub round trip, or the user
// signs in and lands on a page that no longer knows what it was asked.
func TestConsentPageSendsAnonymousVisitorsToLoginAndBack(t *testing.T) {
	s := newOAuthPortal(t, resolveOKAs("alice", "user"))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, authorizeURL(), nil))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303", rec.Code)
	}
	location := rec.Header().Get("Location")
	next, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	back, err := url.Parse(next.Query().Get("next"))
	if err != nil {
		t.Fatal(err)
	}
	if back.Path != "/oauth/authorize" {
		t.Fatalf("login does not return to the consent page: %s", location)
	}
	for _, name := range []string{"client_id", "redirect_uri", "code_challenge", "state", "resource"} {
		if back.Query().Get(name) == "" {
			t.Errorf("%s was dropped on the way to login: %s", name, back)
		}
	}
}

// A request the relay refused must be shown here. Redirecting it back to a URI
// the relay just said it does not recognize is exactly the open redirect the
// registration step exists to prevent.
func TestRefusedRequestIsShownNotRedirected(t *testing.T) {
	s := oauthPortal(t, map[string]any{}, http.StatusBadRequest)
	req := httptest.NewRequest(http.MethodGet, authorizeURL(), nil)
	req.Header.Set("X-User", "alice@example.com")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("a refused request was redirected to %q", loc)
	}
	if !strings.Contains(rec.Body.String(), "unknown client_id") {
		t.Errorf("the page does not say why: %s", rec.Body.String())
	}
}

func decideRequest(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	token := newCSRFToken()
	req := httptest.NewRequest(http.MethodPost, "https://portal.test/api/oauth/decide", strings.NewReader(body))
	req.Header.Set("X-User", "alice@example.com")
	req.Header.Set("Origin", "https://portal.test")
	req.Header.Set(csrfHeaderName, token)
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestDecideApprovesWithTheSignedInNamespace(t *testing.T) {
	seen := map[string]any{}
	s := oauthPortal(t, seen, http.StatusOK)
	rec := decideRequest(t, s, `{"request_id":"req-1","allow":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]string
	json.Unmarshal(rec.Body.Bytes(), &out)
	if !strings.Contains(out["redirect"], "code=abc") {
		t.Fatalf("decide returned %v", out)
	}
	approve, _ := seen["approve"].(map[string]any)
	// The namespace comes from the session, never from the request body: a page
	// must not be able to authorize a connector into somebody else's account.
	if approve["namespace"] != "alice" || approve["request_id"] != "req-1" {
		t.Fatalf("approve body = %v", approve)
	}
}

func TestDecideDenies(t *testing.T) {
	seen := map[string]any{}
	s := oauthPortal(t, seen, http.StatusOK)
	rec := decideRequest(t, s, `{"request_id":"req-1","allow":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if _, approved := seen["approve"]; approved {
		t.Fatal("a denial reached the approve endpoint")
	}
	deny, _ := seen["deny"].(map[string]any)
	if deny["request_id"] != "req-1" {
		t.Fatalf("deny body = %v", deny)
	}
}

// The decision endpoint carries the portal's CSRF protection, so a page the
// user merely visited cannot approve a connector on their behalf.
func TestDecideRefusesCrossSiteAndGET(t *testing.T) {
	s := oauthPortal(t, map[string]any{}, http.StatusOK)

	req := httptest.NewRequest(http.MethodPost, "https://portal.test/api/oauth/decide",
		strings.NewReader(`{"request_id":"req-1","allow":true}`))
	req.Header.Set("X-User", "alice@example.com")
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site POST = %d, want 403", rec.Code)
	}

	rec = httptest.NewRecorder()
	get := httptest.NewRequest(http.MethodGet, "https://portal.test/api/oauth/decide?request_id=req-1&allow=true", nil)
	get.Header.Set("X-User", "alice@example.com")
	s.Handler().ServeHTTP(rec, get)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405", rec.Code)
	}
}

func TestConsentPageRefusesAbsurdlyLongParameters(t *testing.T) {
	s := oauthPortal(t, map[string]any{}, http.StatusOK)
	req := httptest.NewRequest(http.MethodGet,
		"https://portal.test/oauth/authorize?client_id=x&state="+strings.Repeat("s", maxOAuthParam+1), nil)
	req.Header.Set("X-User", "alice@example.com")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
}
