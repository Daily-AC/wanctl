package portal

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequireNSNotWired503(t *testing.T) {
	s := New("", "", "") // no relay URL / secret
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/me", nil)
	req.Header.Set("X-Auth-Request-Email", "someone@x.com")
	s.handleMe(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 when unwired, got %d", rec.Code)
	}
}

func TestNoIdentity401(t *testing.T) {
	s := New("https://relay.example", "secret", "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/me", nil) // no identity header
	s.handleMe(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without identity, got %d", rec.Code)
	}
}

func TestWhoamiDumpsHeaders(t *testing.T) {
	s := New("", "", "X-Auth-Request-Email")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/whoami", nil)
	req.Header.Set("X-Auth-Request-Email", "someone@x.com")
	s.handleWhoami(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "someone@x.com") {
		t.Fatalf("whoami missing identity: %d %s", rec.Code, rec.Body.String())
	}
}
