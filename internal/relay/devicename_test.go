package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"wanctl/internal/transport"
)

const testDeviceID = "11111111-1111-4111-8111-111111111111"

// Names a device may not register under: each would reach a terminal or a
// target through the peer list and mean something other than a name there.
var unprintableNames = map[string]string{
	"escape sequence":   "home\x1b[2Jpc",
	"line break":        "home\npc",
	"nul":               "home\x00pc",
	"C1 control":        "home\u0085pc",
	"namespace slash":   "alice/home-pc",
	"invalid UTF-8":     "home\xffpc",
	"longer than 255 B": string(make([]byte, 256)),
}

// A name that prints as itself is registered as it is, spaces and all.
const printableName = "Lev's MacBook Pro (2) 书房"

func pollRegistration(h http.Handler, q url.Values) int {
	// Already cancelled, so a poll that registers answers at once instead of
	// parking for a job.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/h/poll?"+q.Encode(), nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer tok-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestHTTPRegistrationRefusesUnprintableDeviceNames(t *testing.T) {
	fp := transport.Fingerprint([]byte("device certificate"))
	paths := map[string]func(name string) (url.Values, string){
		"device ID": func(name string) (url.Values, string) {
			return url.Values{"device": {testDeviceID}, "device_id": {testDeviceID}, "name": {name}, "fp": {fp}, "inst": {"A"}}, "alice/" + testDeviceID
		},
		"legacy name": func(name string) (url.Values, string) {
			return url.Values{"device": {"legacy-pc"}, "name": {name}, "fp": {fp}}, "alice/legacy-pc"
		},
	}
	for path, query := range paths {
		t.Run(path, func(t *testing.T) {
			for label, name := range unprintableNames {
				r := New(EnvTokenStore("tok-alice:alice"))
				q, key := query(name)
				if code := pollRegistration(r.Handler(), q); code != http.StatusConflict {
					t.Errorf("%s: registration answered %d, want 409", label, code)
				}
				if r.hagents[key] != nil {
					t.Errorf("%s: the device was registered under %q", label, name)
				}
			}
			r := New(EnvTokenStore("tok-alice:alice"))
			q, key := query(printableName)
			pollRegistration(r.Handler(), q)
			if a := r.hagents[key]; a == nil || a.name != printableName {
				t.Fatalf("a printable name was not registered as it is: %+v", a)
			}
		})
	}
}

func TestWSRegistrationRefusesUnprintableDeviceNames(t *testing.T) {
	fp := transport.Fingerprint([]byte("device certificate"))
	paths := map[string]func(name string) (map[string]string, string){
		"device ID": func(name string) (map[string]string, string) {
			return map[string]string{"op": "register", "device": testDeviceID, "device_id": testDeviceID, "name": name, "fingerprint": fp}, "alice/" + testDeviceID
		},
		"legacy name": func(name string) (map[string]string, string) {
			return map[string]string{"op": "register", "device": "legacy-pc", "name": name, "fingerprint": fp}, "alice/legacy-pc"
		},
	}
	registered := func(r *Relay, key string) *agentConn {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.agents[key]
	}
	for path, message := range paths {
		t.Run(path, func(t *testing.T) {
			for label, name := range unprintableNames {
				if label == "invalid UTF-8" {
					continue // JSON carries it as U+FFFD, which is printable
				}
				r := New(EnvTokenStore("tok-alice:alice"))
				s := newPipeRelayServer(t, r.Handler())
				ctrl, _, err := s.wsDial(context.Background(), "/agent", "tok-alice")
				if err != nil {
					t.Fatal(err)
				}
				reg, key := message(name)
				if err := json.NewEncoder(ctrl).Encode(reg); err != nil {
					t.Fatal(err)
				}
				closed := make(chan error, 1)
				go func() {
					_, err := ctrl.Read(make([]byte, 1))
					closed <- err
				}()
				select {
				case err := <-closed:
					if err == nil {
						t.Errorf("%s: the relay sent something instead of closing", label)
					}
				case <-time.After(5 * time.Second):
					t.Errorf("%s: the relay kept a control channel registered as %q", label, name)
				}
				if registered(r, key) != nil {
					t.Errorf("%s: the device was registered under %q", label, name)
				}
				ctrl.Close()
			}
			r := New(EnvTokenStore("tok-alice:alice"))
			s := newPipeRelayServer(t, r.Handler())
			ctrl, _, err := s.wsDial(context.Background(), "/agent", "tok-alice")
			if err != nil {
				t.Fatal(err)
			}
			defer ctrl.Close()
			reg, key := message(printableName)
			if err := json.NewEncoder(ctrl).Encode(reg); err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool { return registered(r, key) != nil }, "a printable name to register")
			if a := registered(r, key); a.name != printableName {
				t.Fatalf("registered as %q, want %q", a.name, printableName)
			}
		})
	}
}
