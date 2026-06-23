package portal

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequireNSNotWired503(t *testing.T) {
	s := New(Config{RelayAdminURL: "", AdminSecret: "", UserHeader: ""}) // no relay URL / secret
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/me", nil)
	req.Header.Set("X-Auth-Request-Email", "someone@x.com")
	s.handleMe(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 when unwired, got %d", rec.Code)
	}
}

func TestNoIdentity401(t *testing.T) {
	s := New(Config{RelayAdminURL: "https://relay.example", AdminSecret: "secret", UserHeader: ""})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/me", nil) // no identity header
	s.handleMe(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without identity, got %d", rec.Code)
	}
}

func TestWhoamiDumpsHeaders(t *testing.T) {
	s := New(Config{RelayAdminURL: "", AdminSecret: "", UserHeader: "X-Auth-Request-Email"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/whoami", nil)
	req.Header.Set("X-Auth-Request-Email", "someone@x.com")
	s.handleWhoami(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "someone@x.com") {
		t.Fatalf("whoami missing identity: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRequireDeviceRejectsForeign(t *testing.T) {
	// fake relay admin: resolve-user -> "alice"; devices -> only "legion"
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "alice"})
		case "/admin/devices":
			json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{{"name": "legion"}}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer relay.Close()

	s := New(Config{RelayAdminURL: relay.URL, AdminSecret: "x", UserHeader: "X-User"})
	req := httptest.NewRequest("GET", "/api/devices/console?device=evil", nil)
	req.Header.Set("X-User", "alice@corp")
	w := httptest.NewRecorder()
	if ns, ok := s.requireDevice(w, req, "evil"); ok {
		t.Fatalf("expected reject of foreign device, got ns=%s", ns)
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}
}

func TestRequireDeviceAllowsOwn(t *testing.T) {
	// fake relay admin: resolve-user -> "alice"; devices -> only "legion"
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "alice"})
		case "/admin/devices":
			json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{{"name": "legion"}}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer relay.Close()

	s := New(Config{RelayAdminURL: relay.URL, AdminSecret: "x", UserHeader: "X-User"})
	req := httptest.NewRequest("GET", "/api/devices/console?device=legion", nil)
	req.Header.Set("X-User", "alice@corp")
	w := httptest.NewRecorder()
	ns, ok := s.requireDevice(w, req, "legion")
	if !ok {
		t.Fatalf("expected own device to be allowed, got ok=false (status %d)", w.Code)
	}
	if ns != "alice" {
		t.Fatalf("expected ns=alice, got %q", ns)
	}
}
