package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"wanctl/internal/httpconn"
	"wanctl/internal/sessionauth"
)

// A device killed mid-command (SIGKILL, a self-update replacing its process)
// used to leave the controller polling its side of the session, and a polling
// controller is exactly what keeps a session alive: the controller hung until
// someone killed it (2026-09-29). These pin down that the relay ends the
// session once the agent process that took it is gone, says why, and leaves
// alone a session whose device is still there.

// openBound dials alice/home-pc and has the agent instance inst pick the job
// up through a real /h/poll, which is what binds the session to it.
func openBound(t *testing.T, r *Relay, inst string) string {
	t.Helper()
	poll(t, r, inst, true)
	req := httptest.NewRequest(http.MethodGet, "/h/dial?target=alice/home-pc", nil)
	req.Header.Set("Authorization", "Bearer tok-alice")
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dial answered %d: %s", rec.Code, rec.Body)
	}
	var out struct{ Session string }
	json.NewDecoder(rec.Body).Decode(&out)
	rec = poll(t, r, inst, false)
	var open sessionauth.Open
	if err := json.NewDecoder(rec.Body).Decode(&open); err != nil || open.Session != out.Session {
		t.Fatalf("the agent's poll did not carry the job: %q %v", rec.Body, err)
	}
	return out.Session
}

// poll sends one /h/poll as agent instance inst. register=true returns at once
// instead of parking for a job.
func poll(t *testing.T, r *Relay, inst string, register bool) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.Background()
	if register {
		c, cancel := context.WithCancel(ctx)
		cancel()
		ctx = c
	}
	req := httptest.NewRequest(http.MethodGet, "/h/poll?device=home-pc&inst="+inst, nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer tok-alice")
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	return rec
}

func endReason(r *Relay, sid string) (string, bool) {
	r.hmu.Lock()
	defer r.hmu.Unlock()
	s := r.hsess[sid]
	if s == nil {
		return "", false
	}
	return s.endReason, true
}

func TestSessionEndsWhenItsDeviceGoesSilent(t *testing.T) {
	r := New(EnvTokenStore("tok-alice:alice"))
	t.Cleanup(func() { closeAllHTTPSessions(r) })
	sid := openBound(t, r, "one")

	r.endSessionsOfGoneAgents(time.Now())
	if reason, ok := endReason(r, sid); !ok || reason != "" {
		t.Fatalf("a device that just polled lost its session (reason %q, registered %v)", reason, ok)
	}
	r.endSessionsOfGoneAgents(time.Now().Add(deviceGoneAfter + time.Second))
	if reason, _ := endReason(r, sid); reason != sessionEndDeviceGone {
		t.Fatalf("a device silent for %v kept its session (reason %q)", deviceGoneAfter, reason)
	}

	// The controller's next poll says so.
	req := httptest.NewRequest(http.MethodGet, "/h/down?session="+sid+"&role=client&ack=0", nil)
	req.Header.Set("Authorization", "Bearer tok-alice")
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusGone || rec.Header().Get(httpconn.SessionEndHeader) != sessionEndDeviceGone {
		t.Fatalf("controller poll answered %d with %s=%q, want 410 %q", rec.Code, httpconn.SessionEndHeader, rec.Header().Get(httpconn.SessionEndHeader), sessionEndDeviceGone)
	}
	// Having said so, the session goes.
	if _, ok := endReason(r, sid); ok {
		t.Fatal("the session outlived the 410 that ended it")
	}
}

// A device whose registration poll is parked is alive, however long ago that
// poll began: that is the state of every healthy agent between jobs, and of
// one whose command waits on a human approval.
func TestSessionKeptWhileItsDevicePolls(t *testing.T) {
	r := New(EnvTokenStore("tok-alice:alice"))
	t.Cleanup(func() { closeAllHTTPSessions(r) })
	sid := openBound(t, r, "one")
	r.hmu.Lock()
	r.hagents["alice/home-pc"].polls++
	r.hmu.Unlock()
	r.endSessionsOfGoneAgents(time.Now().Add(10 * deviceGoneAfter))
	if reason, ok := endReason(r, sid); !ok || reason != "" {
		t.Fatalf("a device with a poll in flight lost its session (reason %q, registered %v)", reason, ok)
	}
}

// A self-update or a supervisor restart brings the device back as a new
// instance within seconds. The old process's sessions died with it, so they end
// on the next scan rather than after the silence window.
func TestSessionEndsWhenItsDeviceIsReplaced(t *testing.T) {
	r := New(EnvTokenStore("tok-alice:alice"))
	t.Cleanup(func() { closeAllHTTPSessions(r) })
	sid := openBound(t, r, "one")
	poll(t, r, "two", true)
	r.endSessionsOfGoneAgents(time.Now())
	if reason, _ := endReason(r, sid); reason != sessionEndDeviceGone {
		t.Fatalf("the replaced instance's session was kept (reason %q)", reason)
	}
}

// A session no HTTP agent has picked up, or one carried to a WebSocket agent,
// is not judged by the HTTP registry: the socket closing already ends it.
func TestUnboundSessionIsNotJudged(t *testing.T) {
	r := New(EnvTokenStore("tok-alice:alice"))
	registerHTTPAgent(t, r, "tok-alice", "alice/home-pc")
	t.Cleanup(func() { closeAllHTTPSessions(r) })
	code, sid := dialAs(t, r, "tok-alice", "alice/home-pc")
	if code != http.StatusOK {
		t.Fatalf("dial answered %d (%s)", code, sid)
	}
	r.hmu.Lock()
	delete(r.hagents, "alice/home-pc")
	r.hmu.Unlock()
	r.endSessionsOfGoneAgents(time.Now().Add(10 * deviceGoneAfter))
	if reason, ok := endReason(r, sid); !ok || reason != "" {
		t.Fatalf("an unbound session was ended (reason %q, registered %v)", reason, ok)
	}
}
