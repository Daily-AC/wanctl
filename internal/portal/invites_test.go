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
			resp := rr.Result()
			return resp, nil
		}
		return inner.RoundTrip(req)
	})}
	return s
}

func inviteReq(s *Server, method, path, body string, login *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if login != nil {
		for _, c := range login.Result().Cookies() {
			req.AddCookie(c)
		}
	}
	rec := httptest.NewRecorder()
	switch path {
	case "/api/invites/revoke":
		s.handleInviteRevoke(rec, req)
	default:
		s.handleInvites(rec, req)
	}
	return rec
}

func TestInvitesRequireSignIn(t *testing.T) {
	var calls []string
	s := invitesPortal(t, "admin", &calls)
	if rec := inviteReq(s, "GET", "/api/invites", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list: status %d", rec.Code)
	}
	if len(calls) != 0 {
		t.Fatalf("relay was reached without a session: %v", calls)
	}
}

func TestInvitesRejectNonAdmin(t *testing.T) {
	var calls []string
	s := invitesPortal(t, "user", &calls)
	login := loginThroughCallback(t, s, "")
	for _, c := range [][2]string{{"GET", "/api/invites"}, {"POST", "/api/invites"}, {"POST", "/api/invites/revoke"}} {
		if rec := inviteReq(s, c[0], c[1], "{}", login); rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s as plain user: status %d", c[0], c[1], rec.Code)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("relay was reached by a non-admin: %v", calls)
	}
}

func TestInvitesAdminPassThrough(t *testing.T) {
	var calls []string
	s := invitesPortal(t, "admin", &calls)
	login := loginThroughCallback(t, s, "")

	rec := inviteReq(s, "POST", "/api/invites", `{"github_login":""}`, login)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "winv_testcode") {
		t.Fatalf("create: status %d body %q", rec.Code, rec.Body.String())
	}
	rec = inviteReq(s, "GET", "/api/invites", "", login)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":7`) {
		t.Fatalf("list: status %d body %q", rec.Code, rec.Body.String())
	}
	rec = inviteReq(s, "POST", "/api/invites/revoke", `{"id":7}`, login)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: status %d", rec.Code)
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
