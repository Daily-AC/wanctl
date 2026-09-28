package relay

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// registerHTTPAgent registers ns/device as an HTTP agent that is online.
func registerHTTPAgent(t *testing.T, r *Relay, token, key string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // registers and returns instead of parking for a job
	device := key[strings.Index(key, "/")+1:]
	req := httptest.NewRequest(http.MethodGet, "/h/poll?device="+device+"&inst=one", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+token)
	r.Handler().ServeHTTP(httptest.NewRecorder(), req)
	r.hmu.Lock()
	defer r.hmu.Unlock()
	if r.hagents[key] == nil {
		t.Fatalf("%s did not register", key)
	}
}

// dialAs dials target over /h/dial and, when that opened a session on an HTTP
// agent, takes the job off the agent's queue as a polling agent would.
func dialAs(t *testing.T, r *Relay, token, target string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/h/dial?target="+target, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return rec.Code, strings.TrimSpace(rec.Body.String())
	}
	var body struct{ Session string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	r.hmu.Lock()
	a := r.hagents[target]
	r.hmu.Unlock()
	if a != nil {
		<-a.open
	}
	return rec.Code, body.Session
}

func TestSessionCapPerNamespace(t *testing.T) {
	r := New(EnvTokenStore("tok-alice:alice,tok-bob:bob,tok-portal:portal"))
	r.SetPortalNS("portal")
	registerHTTPAgent(t, r, "tok-alice", "alice/home-pc")
	registerHTTPAgent(t, r, "tok-bob", "bob/laptop")
	t.Cleanup(func() {
		r.hmu.Lock()
		all := make(map[string]*httpSession, len(r.hsess))
		for sid, s := range r.hsess {
			all[sid] = s
		}
		r.hmu.Unlock()
		for sid, s := range all {
			r.closeHTTPSession(sid, s)
		}
	})

	var sessions []string
	for i := range httpSessionsPerNS {
		code, sid := dialAs(t, r, "tok-alice", "alice/home-pc")
		if code != http.StatusOK {
			t.Fatalf("dial %d answered %d (%s)", i+1, code, sid)
		}
		sessions = append(sessions, sid)
	}
	code, msg := dialAs(t, r, "tok-alice", "alice/home-pc")
	if code != http.StatusTooManyRequests {
		t.Fatalf("dial %d answered %d, want 429", httpSessionsPerNS+1, code)
	}
	if !strings.Contains(msg, "open sessions") {
		t.Fatalf("the refusal does not say why: %q", msg)
	}

	// Another account is not held to alice's count, and neither is the portal,
	// which opens a console session per device for everyone.
	if code, msg := dialAs(t, r, "tok-bob", "bob/laptop"); code != http.StatusOK {
		t.Fatalf("bob's dial answered %d (%s)", code, msg)
	}
	for i := range httpSessionsPerNS + 8 {
		if code, msg := dialAs(t, r, "tok-portal", "alice/home-pc"); code != http.StatusOK {
			t.Fatalf("portal dial %d answered %d (%s)", i+1, code, msg)
		}
	}

	// A session that ends gives its place back.
	r.closeHTTPSession(sessions[0], r.session(sessions[0]))
	if code, msg := dialAs(t, r, "tok-alice", "alice/home-pc"); code != http.StatusOK {
		t.Fatalf("dial after a session ended answered %d (%s)", code, msg)
	}
	if code, _ := dialAs(t, r, "tok-alice", "alice/home-pc"); code != http.StatusTooManyRequests {
		t.Fatalf("the place was given back twice: %d", code)
	}
}

// Every path that opens an HTTP session is held to the same count: a
// WebSocket controller dialing an HTTP agent, and an HTTP controller dialing
// a WebSocket agent.
func TestSessionCapCoversBridgedSessions(t *testing.T) {
	r := New(EnvTokenStore("tok-alice:alice"))
	s := newPipeRelayServer(t, r.Handler())
	registerHTTPAgent(t, r, "tok-alice", "alice/home-pc")
	ctrl := registerWSAgent(t, r, s, "tok-alice", "ws-pc")
	defer ctrl.Close()
	go io.Copy(io.Discard, ctrl) // the agent takes whatever jobs it is sent
	for range httpSessionsPerNS {
		if code, msg := dialAs(t, r, "tok-alice", "alice/home-pc"); code != http.StatusOK {
			t.Fatalf("dial answered %d (%s)", code, msg)
		}
	}
	t.Cleanup(func() {
		r.hmu.Lock()
		all := make(map[string]*httpSession, len(r.hsess))
		for sid, s := range r.hsess {
			all[sid] = s
		}
		r.hmu.Unlock()
		for sid, s := range all {
			r.closeHTTPSession(sid, s)
		}
	})

	if code, msg := dialAs(t, r, "tok-alice", "alice/ws-pc"); code != http.StatusTooManyRequests {
		t.Fatalf("an HTTP dial to a WebSocket agent answered %d (%s), want 429", code, msg)
	}
	nc, resp, err := s.wsDial(context.Background(), "/dial?target=alice/home-pc", "tok-alice")
	if err == nil {
		nc.Close()
		t.Fatal("a WebSocket dial to an HTTP agent was let through")
	}
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a WebSocket dial to an HTTP agent was refused with %v, want 429", resp)
	}
}

func TestSessionCapRelayWide(t *testing.T) {
	r := New(EnvTokenStore("tok-alice:alice,tok-bob:bob,tok-portal:portal"))
	r.SetPortalNS("portal")
	r.maxSessions = 3
	registerHTTPAgent(t, r, "tok-alice", "alice/home-pc")
	registerHTTPAgent(t, r, "tok-bob", "bob/laptop")
	var opened []string
	for _, d := range []struct{ token, target string }{
		{"tok-alice", "alice/home-pc"}, {"tok-bob", "bob/laptop"}, {"tok-portal", "alice/home-pc"},
	} {
		code, sid := dialAs(t, r, d.token, d.target)
		if code != http.StatusOK {
			t.Fatalf("dial answered %d (%s)", code, sid)
		}
		opened = append(opened, sid)
	}
	for _, token := range []string{"tok-alice", "tok-bob", "tok-portal"} {
		target := "alice/home-pc"
		if token == "tok-bob" {
			target = "bob/laptop"
		}
		if code, _ := dialAs(t, r, token, target); code != http.StatusTooManyRequests {
			t.Fatalf("%s dialed past the relay's limit: %d", token, code)
		}
	}
	r.closeHTTPSession(opened[1], r.session(opened[1]))
	if code, msg := dialAs(t, r, "tok-bob", "bob/laptop"); code != http.StatusOK {
		t.Fatalf("dial after a session ended answered %d (%s)", code, msg)
	}
	for _, sid := range opened {
		r.closeHTTPSession(sid, r.session(sid))
	}
}

// Ordinary use never meets the cap: each command's session leaves as soon as
// both ends have closed it, so running more commands in a row than the cap
// allows open at once is fine.
func TestCommandsInARowStayUnderTheSessionCap(t *testing.T) {
	r := New(EnvTokenStore("tok-alice:alice"))
	registerHTTPAgent(t, r, "tok-alice", "alice/home-pc")
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	hc := &http.Client{Transport: &http.Transport{}}
	cert := testTLSCert(t)
	for i := range httpSessionsPerNS + 6 {
		code, sid := dialAs(t, r, "tok-alice", "alice/home-pc")
		if code != http.StatusOK {
			t.Fatalf("command %d: dial answered %d (%s)", i+1, code, sid)
		}
		client, agent := dialPair(t, srv.URL, sid, hc)
		done := make(chan error, 1)
		go func() {
			device := tls.Server(agent, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
			if _, err := io.ReadFull(device, make([]byte, 4)); err != nil {
				done <- err
				return
			}
			_, err := device.Write([]byte("exit 0"))
			device.Close()
			done <- err
		}()
		controller := tls.Client(client, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
		if _, err := controller.Write([]byte("true")); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(controller, make([]byte, 6)); err != nil {
			t.Fatal(err)
		}
		controller.Close()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}
