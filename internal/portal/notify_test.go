package portal

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNotifyPostUsesSessionNamespace(t *testing.T) {
	var posted map[string]any
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "bob"})
		case "/admin/notify":
			if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
				t.Fatal(err)
			}
			json.NewEncoder(w).Encode(map[string]any{"configured": true})
		default:
			http.NotFound(w, r)
		}
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notify", strings.NewReader(
		`{"namespace":"alice","url":"https://hooks.example/topic","format":"json"}`))
	req.Header.Set("X-User", "bob@example.com")
	rr := httptest.NewRecorder()
	s.handleNotify(rr, req)
	if rr.Code != http.StatusOK || posted["namespace"] != "bob" {
		t.Fatalf("response = %d %q; posted = %+v", rr.Code, rr.Body.String(), posted)
	}
}

func TestSharedDeviceCannotReadOrWriteNotifyHealth(t *testing.T) {
	notifyCalls := 0
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "bob"})
		case "/admin/devices":
			json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{{
				"name": "legion", "owner": "alice", "shared": true,
			}}})
		case "/admin/devices/notify":
			notifyCalls++
			json.NewEncoder(w).Encode(map[string]any{
				"device": "legion", "enabled": true,
				"health": map[string]any{"error": "owner-only-secret"},
			})
		default:
			http.NotFound(w, r)
		}
	})
	requests := []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/devices/notify?device=legion", nil),
		httptest.NewRequest(http.MethodPost, "/api/devices/notify", strings.NewReader(`{"device":"legion","enabled":true}`)),
	}
	for _, req := range requests {
		req.Header.Set("X-User", "bob@example.com")
		rr := httptest.NewRecorder()
		s.handleDeviceNotify(rr, req)
		if rr.Code != http.StatusForbidden || strings.Contains(rr.Body.String(), "owner-only-secret") {
			t.Fatalf("%s response = %d %q", req.Method, rr.Code, rr.Body.String())
		}
	}
	if notifyCalls != 0 {
		t.Fatalf("shared-device notify endpoint reached relay %d times", notifyCalls)
	}
}
