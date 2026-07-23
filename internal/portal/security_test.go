package portal

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerSetsSecurityHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	New(Config{}).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := rec.Header().Get("Content-Security-Policy"); !strings.Contains(got, "frame-ancestors 'none'") {
		t.Fatalf("CSP does not prevent framing: %q", got)
	}
	for name, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
		"X-Frame-Options":        "DENY",
	} {
		if got := rec.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestMutationRejectsCrossOriginBeforeRelay(t *testing.T) {
	called := false
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		called = true
		json.NewEncoder(w).Encode(map[string]string{"namespace": "alice"})
	})
	req := httptest.NewRequest(http.MethodPost, "https://portal.test/api/tokens", strings.NewReader(`{}`))
	req.Header.Set("Origin", "https://evil.test")
	req.Header.Set("X-User", "alice@example.com")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin mutation = %d, want 403", rec.Code)
	}
	if called {
		t.Fatal("cross-origin mutation reached relay")
	}
}

func TestMutationRejectsMissingDoubleSubmitToken(t *testing.T) {
	called := false
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "https://portal.test/api/tokens", strings.NewReader(`{}`))
	req.Header.Set("Origin", "https://portal.test")
	req.Header.Set("X-User", "alice@example.com")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || called {
		t.Fatalf("missing CSRF token = %d, relay called=%v; want 403 before relay", rec.Code, called)
	}
}

func TestMutationRequiresStrictMethod(t *testing.T) {
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/tokens", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /api/tokens = %d, want 405", rec.Code)
	}
}

func TestMutationBodyLimit(t *testing.T) {
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := s.Handler()
	csrf := csrfCookie(t, h)
	body := `{"value":"` + strings.Repeat("x", maxPortalBodyBytes) + `"}`
	req := httptest.NewRequest(http.MethodPost, "https://portal.test/api/tokens", strings.NewReader(body))
	req.Header.Set("Origin", "https://portal.test")
	req.Header.Set("X-CSRF-Token", csrf.Value)
	req.Header.Set("X-User", "alice@example.com")
	req.AddCookie(csrf)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized POST = %d, want 413", rec.Code)
	}
}

func TestSameOriginDoubleSubmitAllowsSPARequest(t *testing.T) {
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "alice"})
		case "/admin/tokens/issue":
			json.NewEncoder(w).Encode(map[string]string{"token": "issued"})
		default:
			http.NotFound(w, r)
		}
	})
	h := s.Handler()
	csrf := csrfCookie(t, h)
	req := httptest.NewRequest(http.MethodPost, "https://portal.test/api/tokens", strings.NewReader(`{}`))
	req.Header.Set("Origin", "https://portal.test")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf.Value)
	req.Header.Set("X-User", "alice@example.com")
	req.AddCookie(csrf)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-origin mutation = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestConfiguredPublicOriginChecksScheme(t *testing.T) {
	s := New(Config{PublicOrigin: "https://portal.test"})
	token := newCSRFToken()
	req := httptest.NewRequest(http.MethodPost, "http://portal.test/api/tokens", strings.NewReader(`{}`))
	req.Header.Set("Origin", "http://portal.test")
	req.Header.Set(csrfHeaderName, token)
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong origin scheme = %d, want 403", rec.Code)
	}
}

func TestSameOriginAcceptsRefererFallback(t *testing.T) {
	s := New(Config{PublicOrigin: "https://portal.test"})
	req := httptest.NewRequest(http.MethodPost, "http://internal/api/tokens", nil)
	req.Header.Set("Referer", "https://portal.test/tokens")
	if !s.sameOrigin(req) {
		t.Fatal("same-origin Referer should be accepted when Origin is absent")
	}
}

func csrfCookie(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "https://portal.test/", nil))
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == csrfCookieName {
			if !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" {
				t.Fatalf("unsafe CSRF cookie attributes: %#v", cookie)
			}
			return cookie
		}
	}
	t.Fatal("GET / did not issue CSRF cookie")
	return nil
}

func TestWhoamiDisabledByDefaultAndRedactsDebugHeaders(t *testing.T) {
	defaultRec := httptest.NewRecorder()
	New(Config{}).Handler().ServeHTTP(defaultRec, httptest.NewRequest(http.MethodGet, "/whoami", nil))
	if defaultRec.Code != http.StatusNotFound {
		t.Fatalf("default /whoami = %d, want 404", defaultRec.Code)
	}

	s := New(Config{DebugWhoami: true, UserHeader: "X-User"})
	req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	req.Header.Set("X-User", "alice@example.com")
	req.Header.Set("Authorization", "Bearer top-secret")
	req.Header.Set("Cookie", "session=top-secret")
	req.Header.Set("X-Access-Token", "another-top-secret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "alice@example.com") {
		t.Fatalf("debug /whoami = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "top-secret") || !strings.Contains(rec.Body.String(), "[REDACTED]") {
		t.Fatalf("debug /whoami leaked sensitive headers: %s", rec.Body.String())
	}
}

func TestMarkdownLinksUseHTTPAllowlist(t *testing.T) {
	b, err := assets.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if strings.Contains(src, `<a href="$2"`) {
		t.Fatal("markdown href is interpolated without validation")
	}
	if !strings.Contains(src, "mdSafeHref") || !strings.Contains(src, "https?:") {
		t.Fatal("markdown renderer has no explicit HTTP(S) URL allowlist")
	}
}
