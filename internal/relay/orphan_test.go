package relay

import (
	"net/http"
	"testing"
	"time"
)

// A controller killed mid-command (SIGKILL, a crash, a dropped link) leaves its
// device parked in a poll on the session. The device's side alone must not
// keep the session registered, or an account that runs commands under a tool
// with a timeout fills its session count and cannot dial at all.
func TestSessionsOfAVanishedControllerAreReaped(t *testing.T) {
	r := New(EnvTokenStore("tok-alice:alice"))
	registerHTTPAgent(t, r, "tok-alice", "alice/home-pc")
	t.Cleanup(func() { closeAllHTTPSessions(r) })

	for i := range httpSessionsPerNS {
		code, sid := dialAs(t, r, "tok-alice", "alice/home-pc")
		if code != http.StatusOK {
			t.Fatalf("dial %d answered %d (%s)", i+1, code, sid)
		}
		r.session(sid).toAgent.beginTake() // the device waits for the next command
	}
	if code, _ := dialAs(t, r, "tok-alice", "alice/home-pc"); code != http.StatusTooManyRequests {
		t.Fatalf("with every place taken a dial answered %d, want 429", code)
	}

	r.reapHTTP(time.Now().Add(httpSessionIdle + time.Second))
	// The device has kept polling all along; the fast-forwarded sweep also
	// expired its registration, so it polls once more before the next dial.
	registerHTTPAgent(t, r, "tok-alice", "alice/home-pc")
	if code, msg := dialAs(t, r, "tok-alice", "alice/home-pc"); code != http.StatusOK {
		t.Fatalf("after the controllers went away a dial answered %d (%s)", code, msg)
	}
}

// A controller that is still polling keeps its session, device poll or not.
func TestALiveControllersSessionIsKept(t *testing.T) {
	r := New(EnvTokenStore("tok-alice:alice"))
	registerHTTPAgent(t, r, "tok-alice", "alice/home-pc")
	t.Cleanup(func() { closeAllHTTPSessions(r) })

	code, sid := dialAs(t, r, "tok-alice", "alice/home-pc")
	if code != http.StatusOK {
		t.Fatalf("dial answered %d (%s)", code, sid)
	}
	s := r.session(sid)
	s.toAgent.beginTake()
	s.toClient.beginTake() // the controller's own poll is in progress
	r.reapHTTP(time.Now().Add(httpSessionIdle + time.Second))
	if r.session(sid) == nil {
		t.Fatal("a session whose controller is polling was reaped")
	}

	// And between polls, a controller heard from recently is not gone.
	s.toClient.endTake()
	r.reapHTTP(time.Now().Add(httpSessionIdle / 2))
	if r.session(sid) == nil {
		t.Fatal("a session whose controller polled moments ago was reaped")
	}
}

func closeAllHTTPSessions(r *Relay) {
	r.hmu.Lock()
	all := make(map[string]*httpSession, len(r.hsess))
	for sid, s := range r.hsess {
		all[sid] = s
	}
	r.hmu.Unlock()
	for sid, s := range all {
		r.closeHTTPSession(sid, s)
	}
}
