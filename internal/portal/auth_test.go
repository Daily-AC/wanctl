package portal

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newOAuthPortal builds an OAuth-mode portal whose outbound HTTP (relay admin,
// GitHub token + user endpoints) is served by fake, in-memory hosts.
//
// resolve fakes the relay's POST /admin/resolve-user; it receives the decoded
// request body and writes the response.
func newOAuthPortal(t *testing.T, resolve func(body map[string]string, w http.ResponseWriter)) *Server {
	t.Helper()
	s := New(Config{
		RelayAdminURL:      "https://relay.test",
		AdminSecret:        "x",
		GitHubClientID:     "client123",
		GitHubClientSecret: "secret456",
		SessionSecret:      strings.Repeat("k", 32),
		GitHubAuthBase:     "https://gh.test",
		GitHubAPIBase:      "https://api.gh.test",
	})
	s.hc = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		rr := httptest.NewRecorder()
		switch {
		case req.URL.Host == "relay.test" && req.URL.Path == "/admin/resolve-user":
			var body map[string]string
			json.NewDecoder(req.Body).Decode(&body)
			resolve(body, rr)
		case req.URL.Host == "gh.test" && req.URL.Path == "/login/oauth/access_token":
			b, _ := io.ReadAll(req.Body)
			form, _ := url.ParseQuery(string(b))
			if form.Get("client_id") != "client123" || form.Get("client_secret") != "secret456" || form.Get("code") != "goodcode" {
				json.NewEncoder(rr).Encode(map[string]string{"error": "bad_verification_code"})
				break
			}
			json.NewEncoder(rr).Encode(map[string]string{"access_token": "gho_test"})
		case req.URL.Host == "api.gh.test" && req.URL.Path == "/user":
			if req.Header.Get("Authorization") != "Bearer gho_test" {
				rr.WriteHeader(http.StatusUnauthorized)
				break
			}
			json.NewEncoder(rr).Encode(map[string]any{"id": 8437, "login": "octocat", "name": "Mona"})
		default:
			rr.WriteHeader(http.StatusNotFound)
		}
		resp := rr.Result()
		if resp.Body == nil {
			resp.Body = io.NopCloser(strings.NewReader(""))
		}
		return resp, nil
	})}
	return s
}

func resolveOKAs(ns, role string) func(map[string]string, http.ResponseWriter) {
	return func(body map[string]string, w http.ResponseWriter) {
		json.NewEncoder(w).Encode(map[string]string{"namespace": ns, "role": role})
	}
}

// loginThroughCallback drives /auth/login then /auth/callback and returns the
// recorder of the callback response (whose cookies carry the session).
func loginThroughCallback(t *testing.T, s *Server, next string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	loginURL := "/auth/login"
	if next != "" {
		loginURL += "?next=" + url.QueryEscape(next)
	}
	s.handleAuthLogin(rec, httptest.NewRequest("GET", loginURL, nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("login: status %d", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || loc.Host != "gh.test" {
		t.Fatalf("login redirect went to %q", rec.Header().Get("Location"))
	}
	state := loc.Query().Get("state")
	if state == "" {
		t.Fatal("login redirect carries no state")
	}
	cb := httptest.NewRequest("GET", "/auth/callback?code=goodcode&state="+url.QueryEscape(state), nil)
	for _, c := range rec.Result().Cookies() {
		cb.AddCookie(c)
	}
	rec2 := httptest.NewRecorder()
	s.handleAuthCallback(rec2, cb)
	return rec2
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			return c
		}
	}
	t.Fatal("no session cookie set")
	return nil
}

func TestOAuthCallbackFullFlow(t *testing.T) {
	var sawResolve map[string]string
	s := newOAuthPortal(t, func(body map[string]string, w http.ResponseWriter) {
		sawResolve = body
		resolveOKAs("octocat", "admin")(body, w)
	})
	rec := loginThroughCallback(t, s, "/enroll")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/enroll" {
		t.Fatalf("callback: status %d location %q", rec.Code, rec.Header().Get("Location"))
	}
	if sawResolve["provider"] != "github" || sawResolve["subject"] != "8437" || sawResolve["login"] != "octocat" {
		t.Fatalf("resolve body = %v", sawResolve)
	}
	c := sessionCookie(t, rec)
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie attributes: HttpOnly=%v SameSite=%v", c.HttpOnly, c.SameSite)
	}
	// The session now authenticates /api/me.
	me := httptest.NewRequest("GET", "/api/me", nil)
	me.AddCookie(c)
	rec2 := httptest.NewRecorder()
	s.handleMe(rec2, me)
	if rec2.Code != http.StatusOK {
		t.Fatalf("/api/me with session: %d %s", rec2.Code, rec2.Body.String())
	}
	var out map[string]string
	json.Unmarshal(rec2.Body.Bytes(), &out)
	if out["namespace"] != "octocat" || out["role"] != "admin" || out["provider"] != "github" {
		t.Fatalf("/api/me = %v", out)
	}
}

func TestOAuthCallbackRejectsWrongState(t *testing.T) {
	s := newOAuthPortal(t, resolveOKAs("octocat", "user"))
	rec := httptest.NewRecorder()
	s.handleAuthLogin(rec, httptest.NewRequest("GET", "/auth/login", nil))
	cb := httptest.NewRequest("GET", "/auth/callback?code=goodcode&state=forged", nil)
	for _, c := range rec.Result().Cookies() {
		cb.AddCookie(c)
	}
	rec2 := httptest.NewRecorder()
	s.handleAuthCallback(rec2, cb)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("forged state: status %d, want 400", rec2.Code)
	}
	// And with no state cookie at all.
	rec3 := httptest.NewRecorder()
	s.handleAuthCallback(rec3, httptest.NewRequest("GET", "/auth/callback?code=goodcode&state=x", nil))
	if rec3.Code != http.StatusBadRequest {
		t.Fatalf("missing state cookie: status %d, want 400", rec3.Code)
	}
}

func TestOAuthLoginRejectsOpenRedirect(t *testing.T) {
	s := newOAuthPortal(t, resolveOKAs("octocat", "user"))
	rec := loginThroughCallback(t, s, "https://evil.example/phish")
	if got := rec.Header().Get("Location"); got != "/" {
		t.Fatalf("external next must collapse to /, got %q", got)
	}
	rec = loginThroughCallback(t, s, "//evil.example")
	if got := rec.Header().Get("Location"); got != "/" {
		t.Fatalf("scheme-relative next must collapse to /, got %q", got)
	}
}

func TestOAuthModeIgnoresIdentityHeader(t *testing.T) {
	s := newOAuthPortal(t, resolveOKAs("octocat", "user"))
	req := httptest.NewRequest("GET", "/api/me", nil)
	req.Header.Set("X-Auth-Request-Email", "smuggled@example.com")
	rec := httptest.NewRecorder()
	s.handleMe(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("header without session in OAuth mode: %d, want 401", rec.Code)
	}
}

func TestSessionCodecTamperAndExpiry(t *testing.T) {
	s := newOAuthPortal(t, resolveOKAs("octocat", "user"))
	now := time.Now()
	p := &principal{Provider: providerGitHub, Subject: "8437", Login: "octocat",
		Issued: now.Unix(), Expires: now.Add(time.Hour).Unix()}
	v, err := s.encodeSession(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.decodeSession(v); err != nil || got.Login != "octocat" {
		t.Fatalf("roundtrip: %v %v", got, err)
	}
	// Tampered payload fails the MAC.
	parts := strings.Split(v, ".")
	tampered := parts[0] + "." + parts[1][:len(parts[1])-2] + "AA" + "." + parts[2]
	if _, err := s.decodeSession(tampered); err == nil {
		t.Fatal("tampered session accepted")
	}
	// A different key rejects it outright.
	other := newOAuthPortal(t, resolveOKAs("octocat", "user"))
	other.sessionKey = []byte(strings.Repeat("z", 32))
	if _, err := other.decodeSession(v); err == nil {
		t.Fatal("session accepted under a different key")
	}
	// Expired claims are dead even with a valid signature.
	p.Expires = now.Add(-time.Minute).Unix()
	v2, _ := s.encodeSession(p)
	if _, err := s.decodeSession(v2); err == nil {
		t.Fatal("expired session accepted")
	}
}

func TestPendingFlowAndRedeem(t *testing.T) {
	admitted := false
	s := newOAuthPortal(t, func(body map[string]string, w http.ResponseWriter) {
		if body["invite_code"] == "winv_good" {
			admitted = true
		}
		if admitted {
			resolveOKAs("octocat", "user")(body, w)
			return
		}
		http.Error(w, pendingInviteBody, http.StatusForbidden)
	})
	rec := loginThroughCallback(t, s, "")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/pending" {
		t.Fatalf("uninvited login should land on /pending, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	c := sessionCookie(t, rec)

	// APIs are closed while pending.
	me := httptest.NewRequest("GET", "/api/me", nil)
	me.AddCookie(c)
	rec2 := httptest.NewRecorder()
	s.handleMe(rec2, me)
	if rec2.Code != http.StatusForbidden || strings.TrimSpace(rec2.Body.String()) != pendingInviteBody {
		t.Fatalf("pending /api/me: %d %q", rec2.Code, rec2.Body.String())
	}

	// A bad code is refused; the good one admits.
	redeem := func(code string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/auth/redeem", strings.NewReader(`{"code":"`+code+`"}`))
		req.AddCookie(c)
		rr := httptest.NewRecorder()
		s.handleAuthRedeem(rr, req)
		return rr
	}
	if rr := redeem("winv_bad"); rr.Code != http.StatusForbidden {
		t.Fatalf("bad code: %d", rr.Code)
	}
	if rr := redeem("winv_good"); rr.Code != http.StatusOK {
		t.Fatalf("good code: %d %s", rr.Code, rr.Body.String())
	}
	rec3 := httptest.NewRecorder()
	me2 := httptest.NewRequest("GET", "/api/me", nil)
	me2.AddCookie(c)
	s.handleMe(rec3, me2)
	if rec3.Code != http.StatusOK {
		t.Fatalf("post-redeem /api/me: %d", rec3.Code)
	}
}

func TestHeaderModeStillResolvesLegacyBody(t *testing.T) {
	var sawResolve map[string]string
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin/resolve-user" {
			json.NewDecoder(r.Body).Decode(&sawResolve)
			json.NewEncoder(w).Encode(map[string]string{"namespace": "alice", "role": "user"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	req := httptest.NewRequest("GET", "/api/me", nil)
	req.Header.Set("X-User", "alice@example.com")
	rec := httptest.NewRecorder()
	s.handleMe(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("header mode /api/me: %d %s", rec.Code, rec.Body.String())
	}
	if sawResolve["identity"] != "alice@example.com" || sawResolve["provider"] != "" {
		t.Fatalf("header mode must keep the legacy resolve body, got %v", sawResolve)
	}
}

func TestIndexRedirectsAnonymousToLoginInOAuthMode(t *testing.T) {
	s := newOAuthPortal(t, resolveOKAs("octocat", "user"))
	rec := httptest.NewRecorder()
	s.handleIndex(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/auth/login") {
		t.Fatalf("anonymous index: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestLogoutClearsSession(t *testing.T) {
	s := newOAuthPortal(t, resolveOKAs("octocat", "user"))
	rec := loginThroughCallback(t, s, "")
	c := sessionCookie(t, rec)
	req := httptest.NewRequest("POST", "/auth/logout", nil)
	req.AddCookie(c)
	rr := httptest.NewRecorder()
	s.handleAuthLogout(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", rr.Code)
	}
	var cleared bool
	for _, sc := range rr.Result().Cookies() {
		if sc.Name == sessionCookieName && sc.MaxAge < 0 && sc.Value == "" {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("logout did not clear the session cookie")
	}
}
