package portal

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A grantee sees a shared device in their list, but the device's console,
// activity log, approval events and Feishu settings belong to the owner. The
// protocol refuses those kinds from a grantee's own session; the portal dials
// with a privileged token and must refuse them itself (audit 2026-08-28,
// SEC-B-01).
func TestSharedDeviceReadsAreOwnerOnly(t *testing.T) {
	relayHits := 0
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "bob"})
		case "/admin/devices":
			json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{{
				"name": "devbox", "owner": "alice", "shared": true, "perms": "read",
			}}})
		default:
			relayHits++
			w.WriteHeader(http.StatusTeapot)
		}
	})
	routes := map[string]http.HandlerFunc{
		"/api/devices/console": s.handleDeviceConsole,
		"/api/devices/logs":    s.handleDeviceLogs,
		"/api/devices/events":  s.handleDeviceEvents,
		"/api/devices/lark":    s.handleDeviceLark,
	}
	for path, h := range routes {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path+"?device=devbox", nil)
		req.Header.Set("X-User", "bob@example.com")
		h(rec, req)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "共享设备只读") {
			t.Errorf("%s for a shared device: status = %d, want 403 from the owner gate (body %q)", path, rec.Code, rec.Body.String())
		}
	}
	if relayHits != 0 {
		t.Fatalf("a shared-device read reached the relay %d times; nothing beyond the device list may be fetched", relayHits)
	}

	// Control: the same routes are not refused to the owner. (They fail later,
	// on the console dial this test does not fake — anything but 403 is fine.)
	owner := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "alice"})
		case "/admin/devices":
			json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{{
				"name": "devbox", "owner": "alice", "shared": false,
			}}})
		case "/admin/devices/lark":
			json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/devices/lark?device=devbox", nil)
	req.Header.Set("X-User", "alice@example.com")
	owner.handleDeviceLark(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("the owner was refused their own device's Feishu settings: %q", rec.Body.String())
	}
}
