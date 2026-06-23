package portal

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDeriveNS(t *testing.T) {
	cases := map[string]string{
		"***REMOVED***@***REMOVED***": "***REMOVED***",
		"Alice.Bo@x.com":            "alice-bo",
		"bob":                       "bob",
		"ou_abc123":                 "ou-abc123",
	}
	for in, want := range cases {
		if got := deriveNS(in); got != want {
			t.Errorf("deriveNS(%q)=%q want %q", in, got, want)
		}
	}
}

func TestRequireNSNoHeader401(t *testing.T) {
	s := &Server{userHeader: "X-Forwarded-User"} // db unused on this path
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/me", nil)
	s.handleMe(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without identity header, got %d", rec.Code)
	}
}

func TestWhoamiDumpsHeaders(t *testing.T) {
	s := &Server{userHeader: "X-Forwarded-User"}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/whoami", nil)
	req.Header.Set("X-Forwarded-User", "someone@x.com")
	s.handleWhoami(rec, req)
	body := rec.Body.String()
	if rec.Code != 200 || !contains(body, "someone@x.com") {
		t.Fatalf("whoami missing identity: %d %s", rec.Code, body)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
