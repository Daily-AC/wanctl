package portal

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// invitesPortal is newOAuthPortal plus fake relay invite endpoints, so the
// pass-through (method, body, response) can be asserted end to end.
func invitesPortal(t *testing.T, role string, relayCalls *[]string) *Server {
	t.Helper()
	s := newOAuthPortal(t, resolveOKAs("alice", role))
	inner := s.hc.Transport
	s.hc = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "relay.test" && strings.HasPrefix(req.URL.Path, "/admin/invites") {
			rr := httptest.NewRecorder()
			var b []byte
			if req.Body != nil {
				b, _ = io.ReadAll(req.Body)
			}
			*relayCalls = append(*relayCalls, req.Method+" "+req.URL.Path+" "+string(b))
			switch {
			case req.URL.Path == "/admin/invites" && req.Method == "POST":
				json.NewEncoder(rr).Encode(map[string]any{"id": 7, "code": "winv_testcode"})
			case req.URL.Path == "/admin/invites":
				json.NewEncoder(rr).Encode([]map[string]any{{"id": 7, "has_code": true}})
			case req.URL.Path == "/admin/invites/revoke":
				rr.WriteHeader(http.StatusOK)
			}
			return rr.Result(), nil
		}
		return inner.RoundTrip(req)
	})}
	return s
}

// inviteSession logs in through the OAuth callback and collects the session
// cookie plus a CSRF cookie minted by the security middleware on a GET.
func inviteSession(t *testing.T, s *Server, h http.Handler) []*http.Cookie {
	t.Helper()
	login := loginThroughCallback(t, s, "")
	cookies := login.Result().Cookies()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "https://portal.test/api/me", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	h.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == csrfCookieName {
			cookies = append(cookies, c)
		}
	}
	return cookies
}

// inviteReq drives the full handler chain — security middleware included, the
// same path the SPA takes — with same-origin and double-submit headers set.
func inviteReq(h http.Handler, method, path, body string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "https://portal.test"+path, strings.NewReader(body))
	req.Header.Set("Origin", "https://portal.test")
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
		if c.Name == csrfCookieName {
			req.Header.Set("X-CSRF-Token", c.Value)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestInvitesRequireSignIn(t *testing.T) {
	var calls []string
	s := invitesPortal(t, "admin", &calls)
	h := s.Handler()
	if rec := inviteReq(h, "GET", "/api/invites", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list: status %d", rec.Code)
	}
	if len(calls) != 0 {
		t.Fatalf("relay was reached without a session: %v", calls)
	}
}

func TestInvitesRejectNonAdmin(t *testing.T) {
	var calls []string
	s := invitesPortal(t, "user", &calls)
	h := s.Handler()
	cookies := inviteSession(t, s, h)
	for _, c := range [][2]string{{"GET", "/api/invites"}, {"POST", "/api/invites"}, {"POST", "/api/invites/revoke"}} {
		if rec := inviteReq(h, c[0], c[1], "{}", cookies); rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s as plain user: status %d body %q", c[0], c[1], rec.Code, rec.Body.String())
		}
	}
	if len(calls) != 0 {
		t.Fatalf("relay was reached by a non-admin: %v", calls)
	}
}

func TestInvitesAdminPassThrough(t *testing.T) {
	var calls []string
	s := invitesPortal(t, "admin", &calls)
	h := s.Handler()
	cookies := inviteSession(t, s, h)

	rec := inviteReq(h, "POST", "/api/invites", `{"github_login":""}`, cookies)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "winv_testcode") {
		t.Fatalf("create: status %d body %q", rec.Code, rec.Body.String())
	}
	rec = inviteReq(h, "GET", "/api/invites", "", cookies)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":7`) {
		t.Fatalf("list: status %d body %q", rec.Code, rec.Body.String())
	}
	rec = inviteReq(h, "POST", "/api/invites/revoke", `{"id":7}`, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: status %d body %q", rec.Code, rec.Body.String())
	}
	want := []string{
		`POST /admin/invites {"github_login":""}`,
		"GET /admin/invites ",
		`POST /admin/invites/revoke {"id":7}`,
	}
	for i, w := range want {
		if i >= len(calls) || calls[i] != w {
			t.Fatalf("relay call %d = %q, want %q (all: %v)", i, calls[i], w, calls)
		}
	}
}
