package portal

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A share that the owner did not mark manageable gives its grantee the device
// but not its control plane: no console state, no approval queue, no rules or
// mode. The portal dials with a privileged token, so it has to apply the rule
// itself rather than rely on the session's capabilities (audit 2026-08-28,
// SEC-B-01, restated for ADR 0007's single switch).
//
// The approval event stream and the activity log are deliberately not in this
// list. They are reads of what the device did, which every grantee already has
// through `wanctl logs`; gating them here would allow in one client what it
// refuses in the other. They have their own case below.
func TestSharedDeviceConsoleNeedsTheManagementSwitch(t *testing.T) {
	relayHits := 0
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "bob"})
		case "/admin/devices":
			json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{{
				"name": "devbox", "owner": "alice", "shared": true, "perms": "full", "manage": false,
			}}})
		default:
			relayHits++
			w.WriteHeader(http.StatusTeapot)
		}
	})
	routes := map[string]http.HandlerFunc{
		"/api/devices/console": s.handleDeviceConsole,
		"/api/devices/lark":    s.handleDeviceLark,
		"/api/devices/notify":  s.handleDeviceNotify,
	}
	for path, h := range routes {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path+"?device=devbox", nil)
		req.Header.Set("X-User", "bob@example.com")
		h(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s without the management switch: status = %d, want 403 (body %q)", path, rec.Code, rec.Body.String())
		}
	}
	if relayHits != 0 {
		t.Fatalf("a refused shared-device read reached the relay %d times; nothing beyond the device list may be fetched", relayHits)
	}
}

// The other half of that decision. A grantee sees what the device did whether
// or not the owner handed over management, because that is a use-right and
// ADR 0007 makes use-rights unconditional. The assertion is only that the gate
// lets the request through: with no relay wired the handler then fails at the
// dial, which is a 502 and not a refusal.
func TestActivityAndEventsComeWithTheShareNotTheSwitch(t *testing.T) {
	for _, manage := range []bool{false, true} {
		s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/admin/resolve-user":
				json.NewEncoder(w).Encode(map[string]string{"namespace": "bob"})
			case "/admin/devices":
				json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{{
					"name": "devbox", "owner": "alice", "shared": true, "perms": "full", "manage": manage,
				}}})
			default:
				w.WriteHeader(http.StatusOK)
			}
		})
		for path, h := range map[string]http.HandlerFunc{
			"/api/devices/logs":   s.handleDeviceLogs,
			"/api/devices/events": s.handleDeviceEvents,
		} {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", path+"?device=devbox", nil)
			req.Header.Set("X-User", "bob@example.com")
			h(rec, req)
			if rec.Code == http.StatusForbidden {
				t.Errorf("%s with manage=%v: refused; the activity log comes with the share, not the switch", path, manage)
			}
		}
	}
}

// With the switch on, the same grantee administers the device — except for the
// owner's notification settings, which are the owner's contact details rather
// than device state and are never part of a share.
func TestManagedShareGetsTheConsoleButNotTheOwnersNotifications(t *testing.T) {
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "bob"})
		case "/admin/devices":
			json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{{
				"name": "devbox", "owner": "alice", "shared": true, "perms": "full", "manage": true,
			}}})
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	})
	// These pass the gate. They fail later on the console dial this test does
	// not fake, so anything but 403 means the gate let them through.
	for path, h := range map[string]http.HandlerFunc{
		"/api/devices/console": s.handleDeviceConsole,
		"/api/devices/logs":    s.handleDeviceLogs,
		"/api/devices/events":  s.handleDeviceEvents,
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path+"?device=devbox", nil)
		req.Header.Set("X-User", "bob@example.com")
		h(rec, req)
		if rec.Code == http.StatusForbidden {
			t.Errorf("%s with the management switch on: still refused (%q)", path, rec.Body.String())
		}
	}
	// These never are.
	for path, h := range map[string]http.HandlerFunc{
		"/api/devices/lark":   s.handleDeviceLark,
		"/api/devices/notify": s.handleDeviceNotify,
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path+"?device=devbox", nil)
		req.Header.Set("X-User", "bob@example.com")
		h(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: a managed share reached the owner's notification settings (status %d, body %q)", path, rec.Code, rec.Body.String())
		}
	}
}

// Control: the owner is refused nothing on their own device.
func TestOwnerIsNotRefusedTheirOwnDeviceSettings(t *testing.T) {
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
