package portal

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// accessPortal is newOAuthPortal plus a fake relay for the access-request
// endpoints, so both what the portal forwards and what it refuses to forward
// can be asserted end to end.
//
// resolve decides what the relay says about the signed-in identity: an
// applicant is precisely someone the relay answers "pending-invite" for.
func accessPortal(t *testing.T, resolve func(map[string]string, http.ResponseWriter), relayCalls *[]string) *Server {
	t.Helper()
	s := newOAuthPortal(t, resolve)
	inner := s.hc.Transport
	s.hc = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "relay.test" || !strings.HasPrefix(req.URL.Path, "/admin/access-requests") {
			return inner.RoundTrip(req)
		}
		rr := httptest.NewRecorder()
		var b []byte
		if req.Body != nil {
			b, _ = io.ReadAll(req.Body)
		}
		*relayCalls = append(*relayCalls, req.Method+" "+req.URL.RequestURI()+" "+string(b))
		switch {
		case req.URL.Path == "/admin/access-requests/status":
			json.NewEncoder(rr).Encode(map[string]any{"status": "none", "can_apply": true})
		case req.URL.Path == "/admin/access-requests" && req.Method == "POST":
			json.NewEncoder(rr).Encode(map[string]any{"id": 1, "status": "pending"})
		case req.URL.Path == "/admin/access-requests":
			json.NewEncoder(rr).Encode(map[string]any{"requests": []map[string]any{
				{"id": 1, "login": "octocat", "status": "pending"},
			}})
		case req.URL.Path == "/admin/access-requests/decide":
			json.NewEncoder(rr).Encode(map[string]any{"id": 1, "status": "approved"})
		}
		return rr.Result(), nil
	})}
	return s
}

// resolvePendingInvite is the relay's answer for someone who has signed in
// with GitHub but has not been admitted: exactly the state the pending page
// and the request form exist for.
func resolvePendingInvite(_ map[string]string, w http.ResponseWriter) {
	w.WriteHeader(http.StatusForbidden)
	io.WriteString(w, pendingInviteBody)
}

// The queue is private. A plain user must not read it or decide on it, and the
// relay must not be reached at all on the way to being told no — mirroring
// TestInvitesRejectNonAdmin, because this is the same secret in a new place.
func TestAccessRequestsRejectNonAdmin(t *testing.T) {
	var calls []string
	s := accessPortal(t, resolveOKAs("alice", "user"), &calls)
	h := s.Handler()
	cookies := inviteSession(t, s, h)
	for _, c := range [][3]string{
		{"GET", "/api/access-requests", ""},
		{"POST", "/api/access-requests/decide", `{"id":1,"decision":"approved"}`},
	} {
		if rec := inviteReq(h, c[0], c[1], c[2], cookies); rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s as a plain user: status %d body %q", c[0], c[1], rec.Code, rec.Body.String())
		}
	}
	if len(calls) != 0 {
		t.Fatalf("relay was reached by a non-admin: %v", calls)
	}
}

func TestAccessRequestsRequireSignIn(t *testing.T) {
	var calls []string
	s := accessPortal(t, resolveOKAs("alice", "admin"), &calls)
	h := s.Handler()
	if rec := inviteReq(h, "GET", "/api/access-requests", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list: status %d", rec.Code)
	}
	// The two writes are stopped one layer earlier, by the double-submit guard:
	// an anonymous caller has no CSRF cookie to echo, so they never reach the
	// handler that would have said 401.
	for _, c := range [][3]string{
		{"POST", "/api/access-requests/decide", `{"id":1,"decision":"approved"}`},
		{"POST", "/auth/request-access", `{"note":"hi"}`},
	} {
		if rec := inviteReq(h, c[0], c[1], c[2], nil); rec.Code != http.StatusForbidden {
			t.Fatalf("anonymous %s %s: status %d, want 403 from the CSRF guard", c[0], c[1], rec.Code)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("relay was reached without a session: %v", calls)
	}
}

// The administrator's two calls, and what reaches the relay: the decision
// carries the deciding namespace, which the client never gets to choose.
func TestAccessRequestsAdminPassThrough(t *testing.T) {
	var calls []string
	s := accessPortal(t, resolveOKAs("alice", "admin"), &calls)
	h := s.Handler()
	cookies := inviteSession(t, s, h)

	rec := inviteReq(h, "GET", "/api/access-requests", "", cookies)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"login":"octocat"`) {
		t.Fatalf("list: status %d body %q", rec.Code, rec.Body.String())
	}
	rec = inviteReq(h, "POST", "/api/access-requests/decide", `{"id":1,"decision":"approved"}`, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: status %d body %q", rec.Code, rec.Body.String())
	}
	want := []string{
		"GET /admin/access-requests ",
		`POST /admin/access-requests/decide {"decided_by":"alice","decision":"approved","id":1}`,
	}
	for i, w := range want {
		if i >= len(calls) || calls[i] != w {
			t.Fatalf("relay call %d = %q, want %q (all: %v)", i, calls[i], w, calls)
		}
	}
}

// The applicant files their own application: the relay is told who is asking
// by the session, never by the body.
func TestAccessRequestUsesTheSessionIdentity(t *testing.T) {
	var calls []string
	s := accessPortal(t, resolvePendingInvite, &calls)
	h := s.Handler()
	cookies := inviteSession(t, s, h)

	// A body that tries to apply as somebody else.
	body := `{"note":"please","login":"someone-else","subject":"1","provider":"header"}`
	rec := inviteReq(h, "POST", "/auth/request-access", body, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("request: status %d body %q", rec.Code, rec.Body.String())
	}
	var create string
	for _, c := range calls {
		if strings.HasPrefix(c, "POST /admin/access-requests ") {
			create = c
		}
	}
	want := `POST /admin/access-requests {"login":"octocat","note":"please","provider":"github","subject":"8437"}`
	if create != want {
		t.Fatalf("relay create call = %q, want %q", create, want)
	}
}

// Someone who already has a namespace has nothing to apply for; their
// application would sit in the queue forever.
func TestAccessRequestRefusedForAdmittedUser(t *testing.T) {
	var calls []string
	s := accessPortal(t, resolveOKAs("alice", "user"), &calls)
	h := s.Handler()
	cookies := inviteSession(t, s, h)
	rec := inviteReq(h, "POST", "/auth/request-access", `{"note":"hi"}`, cookies)
	if rec.Code != http.StatusConflict {
		t.Fatalf("request from an admitted user: status %d body %q", rec.Code, rec.Body.String())
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "POST /admin/access-requests ") {
			t.Fatalf("an admitted user's application reached the relay: %v", calls)
		}
	}
}

// The double-submit guard covers the new write, the same as /auth/redeem: a
// cross-site POST carrying only the session cookie must not file anything.
func TestAccessRequestRejectsMissingCSRF(t *testing.T) {
	var calls []string
	s := accessPortal(t, resolvePendingInvite, &calls)
	h := s.Handler()
	cookies := inviteSession(t, s, h)

	req := httptest.NewRequest("POST", "https://portal.test/auth/request-access", strings.NewReader(`{}`))
	req.Header.Set("Origin", "https://portal.test")
	for _, c := range cookies {
		req.AddCookie(c) // cookies, but no X-CSRF-Token header
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST without the CSRF header: status %d", rec.Code)
	}
	if len(calls) != 0 {
		t.Fatalf("relay was reached without a CSRF token: %v", calls)
	}
}

// The pending page renders one of four states, and which one is a server-side
// fact: an applicant must never be shown a form whose submit would be refused.
func TestPendingPageRendersTheRequestState(t *testing.T) {
	cases := []struct {
		name   string
		status map[string]any
		want   string
	}{
		{"never asked", map[string]any{"status": "none", "can_apply": true}, `data-req="none"`},
		{"waiting", map[string]any{"status": "pending", "can_apply": false}, `data-req="pending"`},
		{"approved", map[string]any{"status": "approved", "can_apply": false}, `data-req="approved"`},
		{"declined", map[string]any{"status": "declined", "can_apply": false}, `data-req="declined"`},
		// Cooled off: the gate says yes again, so the form comes back.
		{"declined long ago", map[string]any{"status": "declined", "can_apply": true}, `data-req="none"`},
	}
	for _, c := range cases {
		var calls []string
		s := accessPortal(t, resolvePendingInvite, &calls)
		status := c.status
		inner := s.hc.Transport
		s.hc = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == "/admin/access-requests/status" {
				rr := httptest.NewRecorder()
				json.NewEncoder(rr).Encode(status)
				return rr.Result(), nil
			}
			return inner.RoundTrip(req)
		})}
		h := s.Handler()
		cookies := inviteSession(t, s, h)
		rec := inviteReq(h, "GET", "/pending", "", cookies)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", c.name, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), c.want) {
			t.Fatalf("%s: page does not carry %s", c.name, c.want)
		}
	}
}

// A relay that cannot be reached must not tell an applicant their application
// does not exist — the page falls back to offering both doors.
func TestPendingPageFallsBackWhenTheRelayIsDown(t *testing.T) {
	var calls []string
	s := accessPortal(t, resolvePendingInvite, &calls)
	inner := s.hc.Transport
	s.hc = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/admin/access-requests/status" {
			rr := httptest.NewRecorder()
			rr.WriteHeader(http.StatusBadGateway)
			return rr.Result(), nil
		}
		return inner.RoundTrip(req)
	})}
	h := s.Handler()
	cookies := inviteSession(t, s, h)
	rec := inviteReq(h, "GET", "/pending", "", cookies)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `data-req="none"`) {
		t.Fatalf("status %d, page missing the form: %q", rec.Code, rec.Body.String())
	}
}
