package portal

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// docsPortal is newOAuthPortal plus fake relay docs admin endpoints, so the
// admin-only gate can be asserted through the full handler chain.
func docsPortal(t *testing.T, role string, relayCalls *[]string) *Server {
	t.Helper()
	s := newOAuthPortal(t, resolveOKAs("alice", role))
	inner := s.hc.Transport
	s.hc = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "relay.test" && strings.HasPrefix(req.URL.Path, "/admin/docs/") {
			rr := httptest.NewRecorder()
			var b []byte
			if req.Body != nil {
				b, _ = io.ReadAll(req.Body)
			}
			*relayCalls = append(*relayCalls, req.Method+" "+req.URL.Path+" "+string(b))
			json.NewEncoder(rr).Encode(map[string]any{"ok": true})
			return rr.Result(), nil
		}
		return inner.RoundTrip(req)
	})}
	return s
}

var docsWriteEndpoints = []string{
	"/api/docs/articles",
	"/api/docs/articles/delete",
	"/api/docs/groups",
	"/api/docs/groups/delete",
}

func TestDocsWriteRequireSignIn(t *testing.T) {
	var calls []string
	s := docsPortal(t, "admin", &calls)
	h := s.Handler()
	// The security middleware (CSRF double-submit) rejects a cookie-less POST
	// with 403 before auth gets to say 401; either way nothing reaches the relay.
	for _, ep := range docsWriteEndpoints {
		rec := inviteReq(h, "POST", ep, "{}", nil)
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Fatalf("anonymous %s: status %d", ep, rec.Code)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("relay was reached without a session: %v", calls)
	}
}

func TestDocsWriteRejectNonAdmin(t *testing.T) {
	var calls []string
	s := docsPortal(t, "user", &calls)
	h := s.Handler()
	cookies := inviteSession(t, s, h)
	for _, ep := range docsWriteEndpoints {
		if rec := inviteReq(h, "POST", ep, "{}", cookies); rec.Code != http.StatusForbidden {
			t.Fatalf("%s as plain user: status %d body %q", ep, rec.Code, rec.Body.String())
		}
	}
	if len(calls) != 0 {
		t.Fatalf("relay was reached by a non-admin: %v", calls)
	}
}

func TestDocsWriteAdminPassThrough(t *testing.T) {
	var calls []string
	s := docsPortal(t, "admin", &calls)
	h := s.Handler()
	cookies := inviteSession(t, s, h)

	rec := inviteReq(h, "POST", "/api/docs/articles", `{"slug":"a","title":"A","body":"x"}`, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("article write: status %d body %q", rec.Code, rec.Body.String())
	}
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "POST /admin/docs/articles ") {
		t.Fatalf("relay calls = %v", calls)
	}
	// The admin's namespace is stamped as the author.
	if !strings.Contains(calls[0], `"author":"alice"`) {
		t.Fatalf("author not stamped: %v", calls)
	}
}
