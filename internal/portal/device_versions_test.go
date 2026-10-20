package portal

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeviceVersionsProxyIncludesReadOnlyShares(t *testing.T) {
	// Shape exercised against PostgreSQL and /admin/devices in relay's
	// TestDeviceVersionsPostgresMigrationAndRegistration.
	const body = `{"devices":[{"name":"pc","alias":"","fingerprint":"fp","last_seen":null,"owner":"bob","shared":true,"display_name":"pc","legacy_name":"pc","perms":"full","manage":false,"wanctl_version":"v0.9.0","opencu_version":"0.3.99","versions_reported_at":"2026-10-20T01:00:00Z","online":false,"wanctl_status":"behind"}],"latest_wanctl_version":"v1.0.0"}`
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			w.Write([]byte(`{"namespace":"alice"}`))
		case "/admin/devices":
			if r.URL.Query().Get("namespace") != "alice" {
				t.Error("wrong namespace")
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(body))
		default:
			t.Errorf("unexpected extra request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	rec := httptest.NewRecorder()
	s.handleDevices(rec, userReq(http.MethodGet, "/api/devices?namespace=bob", nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != body {
		t.Fatalf("proxy: %d %s", rec.Code, rec.Body.String())
	}
}
