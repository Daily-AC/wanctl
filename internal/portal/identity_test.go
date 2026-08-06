package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"wanctl/internal/client"
	"wanctl/internal/transport"
)

// relayFor answers the two admin calls every device handler makes: identity
// resolution and the device list carrying the registered fingerprint.
func relayFor(ns, device, fingerprint string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": ns})
		case "/admin/devices":
			json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{{
				"name": device, "owner": ns, "fingerprint": fingerprint,
			}}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// withDialer gives the portal a console dialer backed by an in-memory pin store.
// Every assertion here stops at the pin check, so the dial target never has to
// be reachable.
func withDialer(t *testing.T, s *Server) *transport.Store {
	t.Helper()
	seed := bytes.Repeat([]byte{7}, 32)
	id, err := transport.IdentityFromSeed(seed, "portal-test")
	if err != nil {
		t.Fatal(err)
	}
	known := transport.NewMemStore()
	s.known = known
	s.dialer = client.NewWith(id, known, "https://relay.invalid", "tok", "http")
	return known
}

func userReq(method, target string, body any) *http.Request {
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, target, nil)
	} else {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, target, bytes.NewReader(b))
	}
	r.Header.Set("X-User", "alice@example.com")
	return r
}

// A changed device identity must reach the browser as an actionable, structured
// 409 — the fingerprints are the whole content of the decision the owner has to
// make, and a 502 with prose left them nowhere to see it.
func TestConsoleIdentityMismatchReturnsBothFingerprints(t *testing.T) {
	pinned := transport.Fingerprint([]byte("old device cert"))
	current := transport.Fingerprint([]byte("reinstalled device cert"))
	s := newTestPortal(relayFor("alice", "build", current))
	known := withDialer(t, s)
	if err := known.Pin("alice/build", pinned, false); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	s.handleDeviceConsole(rec, userReq("GET", "/api/devices/console?device=build", nil))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
	var got identityChanged
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if got.Error != errDeviceIdentityChanged || got.Device != "build" {
		t.Fatalf("body = %+v", got)
	}
	if got.Pinned != pinned || got.Offered != current {
		t.Fatalf("fingerprints = pinned %q offered %q; want %q / %q", got.Pinned, got.Offered, pinned, current)
	}
}

// Any other dial failure keeps its old shape, so the SPA's generic path (and
// anything scripting the API) is unaffected.
func TestConsoleOtherDialFailuresStay502(t *testing.T) {
	s := newTestPortal(relayFor("alice", "build", transport.Fingerprint([]byte("cert"))))
	// no dialer configured -> "portal console not wired"
	rec := httptest.NewRecorder()
	s.handleDeviceConsole(rec, userReq("GET", "/api/devices/console?device=build", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body.String())
	}
}

// Accepting re-pins to the fingerprint the relay currently reports.
func TestIdentityAcceptRepins(t *testing.T) {
	pinned := transport.Fingerprint([]byte("old device cert"))
	current := transport.Fingerprint([]byte("reinstalled device cert"))
	s := newTestPortal(relayFor("alice", "build", current))
	known := withDialer(t, s)
	known.Pin("alice/build", pinned, false)

	rec := httptest.NewRecorder()
	s.handleDeviceIdentityAccept(rec, userReq("POST", "/api/devices/identity/accept",
		map[string]string{"device": "build", "fingerprint": current}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	p, ok := known.GetByName("alice/build")
	if !ok || p.Fingerprint != current {
		t.Fatalf("pin = %+v (found=%v), want %q", p, ok, current)
	}
}

// A tab left open across a second identity change must not turn one human
// confirmation into a standing permission to pin whatever is claimed later.
func TestIdentityAcceptRejectsStaleFingerprint(t *testing.T) {
	pinned := transport.Fingerprint([]byte("old device cert"))
	shown := transport.Fingerprint([]byte("what the browser displayed"))
	current := transport.Fingerprint([]byte("what the relay claims now"))
	s := newTestPortal(relayFor("alice", "build", current))
	known := withDialer(t, s)
	known.Pin("alice/build", pinned, false)

	rec := httptest.NewRecorder()
	s.handleDeviceIdentityAccept(rec, userReq("POST", "/api/devices/identity/accept",
		map[string]string{"device": "build", "fingerprint": shown}))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
	if p, _ := known.GetByName("alice/build"); p.Fingerprint != pinned {
		t.Fatalf("pin changed to %q; stale confirmations must not re-pin", p.Fingerprint)
	}
}

// Unbinding is the owner stating the name no longer refers to that machine. If
// the pin outlives it, reinstalling under the same name is locked out of the
// portal forever with no UI able to clear it — the bug this fixes.
func TestDeviceRemoveForgetsPin(t *testing.T) {
	fp := transport.Fingerprint([]byte("device cert"))
	removed := false
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin/devices/remove" {
			removed = true
			w.WriteHeader(http.StatusOK)
			return
		}
		relayFor("alice", "build", fp)(w, r)
	})
	known := withDialer(t, s)
	known.Pin("alice/build", fp, false)

	rec := httptest.NewRecorder()
	s.handleDeviceRemove(rec, userReq("POST", "/api/devices/remove", map[string]string{"device": "build"}))

	if rec.Code != http.StatusOK || !removed {
		t.Fatalf("status = %d relay-called = %v; body = %s", rec.Code, removed, rec.Body.String())
	}
	if p, ok := known.GetByName("alice/build"); ok {
		t.Fatalf("pin survived unbind: %+v", p)
	}
}

// One physical device is routinely pinned under several names; forgetting one
// name must not silently forget the others.
func TestForgetPinIsNameScoped(t *testing.T) {
	fp := transport.Fingerprint([]byte("one machine, two names"))
	s := New(Config{})
	known := transport.NewMemStore()
	s.known = known
	known.Pin("alice/build", fp, false)
	known.Pin("alice/build-old", fp, false)

	s.forgetPin("alice", "build")

	if _, ok := known.GetByName("alice/build"); ok {
		t.Fatal("alice/build pin survived")
	}
	if _, ok := known.GetByName("alice/build-old"); !ok {
		t.Fatal("alice/build-old was collaterally forgotten")
	}
}

// deviceConnFor must keep the mismatch inspectable through its wrapping, or the
// handler's errors.As branch silently degrades to a 502.
func TestDeviceConnForWrapsMismatchInspectably(t *testing.T) {
	pinned := transport.Fingerprint([]byte("old"))
	current := transport.Fingerprint([]byte("new"))
	s := newTestPortal(relayFor("alice", "build", current))
	known := withDialer(t, s)
	known.Pin("alice/build", pinned, false)

	_, err := s.deviceConnFor(context.Background(), "alice", "build")
	var mismatch *transport.MismatchError
	if err == nil || !errors.As(err, &mismatch) {
		t.Fatalf("err = %v, want a wrapped *transport.MismatchError", err)
	}
}
