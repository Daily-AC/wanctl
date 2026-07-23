package portal

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func newTestPortal(handler http.HandlerFunc) *Server {
	s := New(Config{RelayAdminURL: "https://relay.test", AdminSecret: "x", UserHeader: "X-User"})
	s.hc = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		rr := httptest.NewRecorder()
		handler(rr, req)
		resp := rr.Result()
		if resp.Body == nil {
			resp.Body = io.NopCloser(strings.NewReader(""))
		}
		return resp, nil
	})}
	return s
}

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

func TestRequireNSPreservesIdentityConflict(t *testing.T) {
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/resolve-user" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		http.Error(w, "derived namespace is already owned by another SSO identity: alice", http.StatusConflict)
	})
	req := httptest.NewRequest("GET", "/api/me", nil)
	req.Header.Set("X-User", "alice@new.example")
	rr := httptest.NewRecorder()

	s.handleMe(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "already owned") {
		t.Fatalf("conflict reason missing from portal response: %q", rr.Body.String())
	}
}

func TestWhoamiDumpsHeadersWhenDebugEnabled(t *testing.T) {
	s := New(Config{RelayAdminURL: "", AdminSecret: "", UserHeader: "X-Auth-Request-Email", DebugWhoami: true})
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
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "alice"})
		case "/admin/devices":
			json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{{"name": "legion"}}})
		default:
			w.WriteHeader(404)
		}
	})

	req := httptest.NewRequest("GET", "/api/devices/console?device=evil", nil)
	req.Header.Set("X-User", "alice@corp")
	w := httptest.NewRecorder()
	if ns, _, ok := s.requireDevice(w, req, "evil"); ok {
		t.Fatalf("expected reject of foreign device, got ns=%s", ns)
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}
}

func TestRequireDeviceAllowsOwn(t *testing.T) {
	// fake relay admin: resolve-user -> "alice"; devices -> only "legion"
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "alice"})
		case "/admin/devices":
			json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{{"name": "legion"}}})
		default:
			w.WriteHeader(404)
		}
	})

	req := httptest.NewRequest("GET", "/api/devices/console?device=legion", nil)
	req.Header.Set("X-User", "alice@corp")
	w := httptest.NewRecorder()
	ns, shared, ok := s.requireDevice(w, req, "legion")
	if !ok {
		t.Fatalf("expected own device to be allowed, got ok=false (status %d)", w.Code)
	}
	if ns != "alice" {
		t.Fatalf("expected ns=alice, got %q", ns)
	}
	if shared {
		t.Fatalf("expected own device shared=false")
	}
}

func TestRequireDeviceReturnsSharedOwnerNamespace(t *testing.T) {
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "bob"})
		case "/admin/devices":
			json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{
				{"name": "zyl", "owner": "***REMOVED***", "shared": true, "perms": "exec"},
			}})
		default:
			w.WriteHeader(404)
		}
	})

	req := httptest.NewRequest("GET", "/api/devices/console?device=zyl", nil)
	req.Header.Set("X-User", "bob@corp")
	w := httptest.NewRecorder()
	ns, shared, ok := s.requireDevice(w, req, "zyl")
	if !ok {
		t.Fatalf("expected shared device to be allowed, got ok=false (status %d body %s)", w.Code, w.Body.String())
	}
	if ns != "***REMOVED***" {
		t.Fatalf("expected owner namespace ***REMOVED***, got %q", ns)
	}
	if !shared {
		t.Fatalf("expected shared=true")
	}
}

func TestRequireDeviceRejectsAmbiguousSharedName(t *testing.T) {
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "bob"})
		case "/admin/devices":
			json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{
				{"name": "build", "owner": "alice", "shared": true},
				{"name": "build", "owner": "carol", "shared": true},
			}})
		default:
			w.WriteHeader(404)
		}
	})

	req := httptest.NewRequest("GET", "/api/devices/console?device=build", nil)
	req.Header.Set("X-User", "bob@corp")
	w := httptest.NewRecorder()
	if ns, _, ok := s.requireDevice(w, req, "build"); ok {
		t.Fatalf("expected ambiguous shared device to be rejected, got ns=%s", ns)
	}
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d body %s", w.Code, w.Body.String())
	}
}

func TestHandleDeviceRemoveRejectsSharedDevice(t *testing.T) {
	var removeCalled bool
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "bob"})
		case "/admin/devices":
			json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{
				{"name": "zyl", "owner": "***REMOVED***", "shared": true},
			}})
		case "/admin/devices/remove":
			removeCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(404)
		}
	})

	req := httptest.NewRequest("POST", "/api/devices/remove", strings.NewReader(`{"device":"zyl"}`))
	req.Header.Set("X-User", "bob@corp")
	w := httptest.NewRecorder()
	s.handleDeviceRemove(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d body %s", w.Code, w.Body.String())
	}
	if removeCalled {
		t.Fatalf("shared device remove should not be forwarded to relay")
	}
}

func TestHandleDeviceRemoveAllowsOwnedDevice(t *testing.T) {
	var removedNamespace, removedDevice string
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "alice"})
		case "/admin/devices":
			json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{
				{"name": "legion", "owner": "alice", "shared": false},
			}})
		case "/admin/devices/remove":
			var body struct{ Namespace, Device string }
			json.NewDecoder(r.Body).Decode(&body)
			removedNamespace, removedDevice = body.Namespace, body.Device
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(404)
		}
	})

	req := httptest.NewRequest("POST", "/api/devices/remove", strings.NewReader(`{"device":"legion"}`))
	req.Header.Set("X-User", "alice@corp")
	w := httptest.NewRecorder()
	s.handleDeviceRemove(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body %s", w.Code, w.Body.String())
	}
	if removedNamespace != "alice" || removedDevice != "legion" {
		t.Fatalf("remove forwarded wrong target: namespace=%q device=%q", removedNamespace, removedDevice)
	}
}

func TestConsoleWriteEndpointsRejectSharedDevice(t *testing.T) {
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "bob"})
		case "/admin/devices":
			json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{
				{"name": "zyl", "owner": "***REMOVED***", "shared": true, "perms": "exec"},
			}})
		default:
			w.WriteHeader(404)
		}
	})

	endpoints := []struct {
		name string
		h    http.HandlerFunc
		body string
	}{
		{"decide", s.handleDeviceDecide, `{"device":"zyl","id":"req1","verdict":"y"}`},
		{"pair", s.handleDevicePair, `{"device":"zyl","fp":"fp1","verdict":"y"}`},
		{"untrust", s.handleDeviceUntrust, `{"device":"zyl","fp":"fp1"}`},
		{"rules", s.handleDeviceRules, `{"device":"zyl","op":"add","kind":"exec","pattern":"echo *"}`},
		{"mode", s.handleDeviceMode, `{"device":"zyl","mode":"bypass"}`},
		{"lan", s.handleDeviceLan, `{"device":"zyl","on":true}`},
	}
	for _, e := range endpoints {
		req := httptest.NewRequest("POST", "/api/devices/"+e.name, strings.NewReader(e.body))
		req.Header.Set("X-User", "bob@corp")
		w := httptest.NewRecorder()
		e.h(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s: shared device must be read-only, want 403, got %d body %s", e.name, w.Code, w.Body.String())
		}
	}
}

func TestConsoleWriteEndpointsPassOwnerGate(t *testing.T) {
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "alice"})
		case "/admin/devices":
			json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{
				{"name": "legion", "owner": "alice", "shared": false},
			}})
		default:
			w.WriteHeader(404)
		}
	})

	// Owner passes the read-only gate; with no console dialer wired the handler
	// then fails downstream with 502 — the point is it is NOT the 403 gate.
	req := httptest.NewRequest("POST", "/api/devices/mode", strings.NewReader(`{"device":"legion","mode":"bypass"}`))
	req.Header.Set("X-User", "alice@corp")
	w := httptest.NewRecorder()
	s.handleDeviceMode(w, req)
	if w.Code == http.StatusForbidden {
		t.Fatalf("owner must not be blocked by the read-only gate, got 403 body %s", w.Body.String())
	}
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 (console not wired) after passing the gate, got %d body %s", w.Code, w.Body.String())
	}
}
