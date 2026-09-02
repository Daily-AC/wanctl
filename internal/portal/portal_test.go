package portal

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wanctl/internal/transport"
)

func TestRegisteredFingerprintUsesRelayAdminRecord(t *testing.T) {
	fp := transport.Fingerprint([]byte("registered device cert"))
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/devices" || r.URL.Query().Get("namespace") != "alice" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{{
			"name": "build", "owner": "alice", "fingerprint": fp,
		}}})
	})
	got, err := s.registeredFingerprint(context.Background(), "alice", "build")
	if err != nil {
		t.Fatal(err)
	}
	if got != fp {
		t.Fatalf("fingerprint = %q, want %q", got, fp)
	}
}

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

func TestSkillsRedirectUsesRelayDialURL(t *testing.T) {
	s := New(Config{RelayDialURL: "wss://relay.example/base/"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/skills", nil)

	s.handleSkills(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "https://relay.example/base/skills" {
		t.Fatalf("Location = %q", got)
	}
}

func TestSkillsRedirectWithoutRelayReturns503(t *testing.T) {
	s := New(Config{})
	rec := httptest.NewRecorder()
	s.handleSkills(rec, httptest.NewRequest(http.MethodGet, "/skills", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "WANCTL_RELAY") {
		t.Fatalf("body = %q", rec.Body.String())
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
				{"name": "devbox", "owner": "alice", "shared": true, "perms": "exec"},
			}})
		default:
			w.WriteHeader(404)
		}
	})

	req := httptest.NewRequest("GET", "/api/devices/console?device=devbox", nil)
	req.Header.Set("X-User", "bob@corp")
	w := httptest.NewRecorder()
	ns, shared, ok := s.requireDevice(w, req, "devbox")
	if !ok {
		t.Fatalf("expected shared device to be allowed, got ok=false (status %d body %s)", w.Code, w.Body.String())
	}
	if ns != "alice" {
		t.Fatalf("expected owner namespace alice, got %q", ns)
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
				{"name": "devbox", "owner": "alice", "shared": true},
			}})
		case "/admin/devices/remove":
			removeCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(404)
		}
	})

	req := httptest.NewRequest("POST", "/api/devices/remove", strings.NewReader(`{"device":"devbox"}`))
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

func TestHandleDeviceAliasRejectsSharedDevice(t *testing.T) {
	var aliasCalled bool
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "bob"})
		case "/admin/devices":
			json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{
				{"name": "devbox", "owner": "alice", "shared": true},
			}})
		case "/admin/devices/alias":
			aliasCalled = true
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/api/devices/alias", strings.NewReader(`{"device":"devbox","alias":"office"}`))
	req.Header.Set("X-User", "bob@corp")
	rr := httptest.NewRecorder()
	s.handleDeviceAlias(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s; want 403", rr.Code, rr.Body.String())
	}
	if aliasCalled {
		t.Fatal("shared-device alias update was forwarded to relay")
	}
}

func TestHandleDeviceAliasForOwnedDevicePassesThroughRelayResponse(t *testing.T) {
	var forwarded struct {
		Namespace string `json:"namespace"`
		Device    string `json:"device"`
		Alias     string `json:"alias"`
	}
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "alice"})
		case "/admin/devices":
			json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{
				{"name": "legion", "owner": "alice", "shared": false},
			}})
		case "/admin/devices/alias":
			json.NewDecoder(r.Body).Decode(&forwarded)
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte("alias_taken"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/api/devices/alias", strings.NewReader(`{"device":"legion","alias":"desk"}`))
	req.Header.Set("X-User", "alice@corp")
	rr := httptest.NewRecorder()
	s.handleDeviceAlias(rr, req)
	if rr.Code != http.StatusConflict || rr.Body.String() != "alias_taken" {
		t.Fatalf("response = %d %q; want relay's 409 alias_taken", rr.Code, rr.Body.String())
	}
	if forwarded.Namespace != "alice" || forwarded.Device != "legion" || forwarded.Alias != "desk" {
		t.Fatalf("forwarded body = %+v", forwarded)
	}
}

func TestConsoleWriteEndpointsRejectSharedDevice(t *testing.T) {
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "bob"})
		case "/admin/devices":
			json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{
				{"name": "devbox", "owner": "alice", "shared": true, "perms": "exec"},
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
		{"decide", s.handleDeviceDecide, `{"device":"devbox","id":"req1","verdict":"y"}`},
		{"pair", s.handleDevicePair, `{"device":"devbox","fp":"fp1","verdict":"y"}`},
		{"untrust", s.handleDeviceUntrust, `{"device":"devbox","fp":"fp1"}`},
		{"rules", s.handleDeviceRules, `{"device":"devbox","op":"add","kind":"exec","pattern":"echo *"}`},
		{"mode", s.handleDeviceMode, `{"device":"devbox","mode":"bypass"}`},
		{"lan", s.handleDeviceLan, `{"device":"devbox","on":true}`},
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

// A docs slug containing an encoded traversal must not reach the relay: it
// once turned the portal into a GET proxy onto the relay's admin origin
// (audit 2026-08-28, SEC-C-03).
func TestDocsArticleRejectsTraversalSlug(t *testing.T) {
	reached := false
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	for _, bad := range []string{"../healthz", "../../admin/logs", "a/b", "UPPER", "sp ace", "dot.dot"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/docs/article/placeholder", nil)
		req.URL.Path = "/api/docs/article/" + bad // the already-decoded path a proxy would hand us
		s.handleDocsArticleGet(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("slug %q: status %d, want 404", bad, rec.Code)
		}
	}
	if reached {
		t.Fatal("a bad slug reached the relay")
	}
	// A legitimate slug still passes through.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/docs/article/quickstart__enroll-device", nil)
	s.handleDocsArticleGet(rec, req)
	if !reached {
		t.Fatal("a valid slug did not reach the relay")
	}
}
