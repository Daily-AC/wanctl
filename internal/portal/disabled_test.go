package portal

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A session whose account the operator disabled is refused on the next
// request, page or API, and cannot file an application to get back in; the
// same session works again once the relay stops saying so.
func TestDisabledAccountIsRefusedEverywhere(t *testing.T) {
	disabled := false
	s := newOAuthPortal(t, func(body map[string]string, w http.ResponseWriter) {
		if disabled {
			http.Error(w, accountDisabledBody, http.StatusForbidden)
			return
		}
		resolveOKAs("octocat", "user")(body, w)
	})
	c := sessionCookie(t, loginThroughCallback(t, s, ""))
	get := func(path string, h http.HandlerFunc) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(c)
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}
	if rec := get("/api/me", s.handleMe); rec.Code != http.StatusOK {
		t.Fatalf("before: /api/me answered %d", rec.Code)
	}

	disabled = true
	if rec := get("/api/me", s.handleMe); rec.Code != http.StatusForbidden || strings.TrimSpace(rec.Body.String()) != accountDisabledBody {
		t.Fatalf("/api/me for a disabled account: %d %q", rec.Code, rec.Body.String())
	}
	page := func(w http.ResponseWriter, r *http.Request) { s.pageAuth(w, r, "/") }
	if rec := get("/", page); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "disabled") {
		t.Fatalf("a page load for a disabled account: %d %q", rec.Code, rec.Body.String())
	}
	if rec := get("/pending", s.handlePending); rec.Code != http.StatusForbidden {
		t.Fatalf("/pending offered a disabled account the application form: %d", rec.Code)
	}

	disabled = false
	if rec := get("/api/me", s.handleMe); rec.Code != http.StatusOK {
		t.Fatalf("after enabling: /api/me answered %d", rec.Code)
	}
}
