package portal

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeviceLarkGetReturnsPersistedConfig(t *testing.T) {
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "alice"})
		case "/admin/devices":
			json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{{
				"name": "legion", "owner": "alice", "shared": false,
			}}})
		case "/admin/devices/lark":
			if r.URL.Query().Get("namespace") != "alice" {
				t.Errorf("lark namespace = %q, want alice", r.URL.Query().Get("namespace"))
			}
			json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{{
				"device": "legion", "approval_enabled": true, "pairing_from_card": true,
				"notify_email": "alice@example.com", "updated_at": "2026-07-30T10:00:00Z",
			}}})
		default:
			http.NotFound(w, r)
		}
	})
	req := httptest.NewRequest(http.MethodGet, "/api/devices/lark?device=legion", nil)
	req.Header.Set("X-User", "alice@example.com")
	rr := httptest.NewRecorder()

	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rr.Code, rr.Body.String())
	}
	var got deviceLarkApproval
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Device != "legion" || !got.ApprovalEnabled || !got.PairingFromCard || got.NotifyEmail != "alice@example.com" {
		t.Fatalf("config = %+v", got)
	}
}

func TestDeviceLarkGetDefaultsOffWithoutRecord(t *testing.T) {
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "alice"})
		case "/admin/devices":
			json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{{"name": "legion"}}})
		case "/admin/devices/lark":
			json.NewEncoder(w).Encode(map[string]any{"devices": []any{}})
		default:
			http.NotFound(w, r)
		}
	})
	req := httptest.NewRequest(http.MethodGet, "/api/devices/lark?device=legion", nil)
	req.Header.Set("X-User", "alice@example.com")
	rr := httptest.NewRecorder()

	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rr.Code, rr.Body.String())
	}
	var got deviceLarkApproval
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Device != "legion" || got.ApprovalEnabled || got.PairingFromCard || got.NotifyEmail != "" {
		t.Fatalf("default config = %+v, want both switches off", got)
	}
}

func TestDeviceLarkPostUsesSSOEmailAndIgnoresBodyEmail(t *testing.T) {
	var persisted map[string]any
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "alice"})
		case "/admin/devices":
			json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{{
				"name": "legion", "owner": "alice", "shared": false,
			}}})
		case "/admin/devices/lark":
			if r.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", r.Method)
			}
			if r.Header.Get("X-Admin-Secret") != "x" {
				t.Error("missing relay admin secret")
			}
			if err := json.NewDecoder(r.Body).Decode(&persisted); err != nil {
				t.Fatal(err)
			}
			persisted["updated_at"] = "2026-07-30T10:00:00Z"
			json.NewEncoder(w).Encode(persisted)
		default:
			http.NotFound(w, r)
		}
	})
	wake := make(chan struct{}, 1)
	s.larkRuntime = &larkRuntime{supervisor: &larkSupervisor{wake: wake}}
	h := s.Handler()
	csrf := csrfCookie(t, h)
	req := httptest.NewRequest(http.MethodPost, "https://portal.test/api/devices/lark", strings.NewReader(
		`{"device":"legion","approval_enabled":true,"pairing_from_card":true,"notify_email":"attacker@example.com"}`))
	req.Header.Set("Origin", "https://portal.test")
	req.Header.Set(csrfHeaderName, csrf.Value)
	req.Header.Set("X-User", "alice@example.com")
	req.AddCookie(csrf)
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rr.Code, rr.Body.String())
	}
	if persisted["namespace"] != "alice" || persisted["device"] != "legion" {
		t.Fatalf("persisted target = %+v", persisted)
	}
	if persisted["notify_email"] != "alice@example.com" {
		t.Fatalf("notify_email = %v, want SSO identity; body spoof was not ignored", persisted["notify_email"])
	}
	if persisted["approval_enabled"] != true || persisted["pairing_from_card"] != true {
		t.Fatalf("persisted switches = %+v", persisted)
	}
	select {
	case <-wake:
	default:
		t.Fatal("successful local switch write did not trigger immediate lark reconcile")
	}
}

func TestDeviceLarkPostRejectsSharedDevice(t *testing.T) {
	larkCalled := false
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "bob"})
		case "/admin/devices":
			json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{{
				"name": "legion", "owner": "alice", "shared": true,
			}}})
		case "/admin/devices/lark":
			larkCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	})
	req := httptest.NewRequest(http.MethodPost, "/api/devices/lark", strings.NewReader(
		`{"device":"legion","approval_enabled":true,"pairing_from_card":false}`))
	req.Header.Set("X-User", "bob@example.com")
	rr := httptest.NewRecorder()

	s.handleDeviceLark(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rr.Code, rr.Body.String())
	}
	if larkCalled {
		t.Fatal("shared-device write reached relay lark endpoint")
	}
}
