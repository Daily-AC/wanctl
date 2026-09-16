package relay

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// /mcp is the canonical path and /wanctl-mcp the alias for edges that reserve
// the /mcp prefix. Both must reach the one handler: mcp.Handler() resets the
// package's session map, so a second handler would split sessions in two.
func TestMCPRoutesShareOneHandler(t *testing.T) {
	var hits int
	r := New(EnvTokenStore("tok:alice"))
	r.SetMCPHandler(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits++
		w.WriteHeader(http.StatusNoContent)
	}))
	h := r.Handler()

	for _, path := range []string{"/mcp", "/mcp/session/abc", "/wanctl-mcp", "/wanctl-mcp/session/abc"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", path, nil))
		if rec.Code != http.StatusNoContent {
			t.Errorf("%s: status %d, want %d", path, rec.Code, http.StatusNoContent)
		}
	}
	if hits != 4 {
		t.Errorf("handler saw %d requests, want 4", hits)
	}
}

// Without a seed the relay never installs the handler, and neither path may
// start answering on its own.
func TestMCPRoutesAbsentWhenDisabled(t *testing.T) {
	h := New(EnvTokenStore("tok:alice")).Handler()
	for _, path := range []string{"/mcp", "/wanctl-mcp"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, rec.Code)
		}
	}
}
