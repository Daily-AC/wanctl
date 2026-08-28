package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// One namespace cannot grow the HTTP registry without bound by polling a fresh
// device name each time (audit 2026-08-28, SEC-A-01).
func TestHTTPPollCapsDistinctDevicesPerNamespace(t *testing.T) {
	r := New(EnvTokenStore("tok:teamA"))
	h := r.Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	poll := func(device string) int {
		req, _ := http.NewRequest("GET", srv.URL+"/h/poll?device="+device, nil)
		req.Header.Set("Authorization", "Bearer tok")
		ctx, cancel := context.WithTimeout(req.Context(), 60*time.Millisecond)
		defer cancel()
		resp, err := http.DefaultClient.Do(req.WithContext(ctx))
		if err != nil {
			return 0
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for i := 0; i < httpAgentsPerNS; i++ {
		poll("dev" + itoa(i))
	}
	if got := poll("one-too-many"); got != http.StatusTooManyRequests {
		t.Fatalf("device past the cap: status %d, want 429", got)
	}
	r.hmu.Lock()
	n := r.countNamespaceAgentsLocked("teamA")
	r.hmu.Unlock()
	if n > httpAgentsPerNS {
		t.Fatalf("registry holds %d entries, cap is %d", n, httpAgentsPerNS)
	}
}

// A token holder who learns a session id in another namespace still cannot
// inject into it or tear it down (audit 2026-08-28, SEC-A-02).
func TestHTTPSessionRefusesOutsideParty(t *testing.T) {
	r := New(EnvTokenStore("ctl:alice,spy:mallory,own:alice"))
	r.hsess["sid1"] = &httpSession{
		toClient: newSideQueue(), toAgent: newSideQueue(),
		callerNS: "alice", ownerNS: "alice", lastActive: time.Now(),
	}
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	req := func(method, path, token string) int {
		rq, _ := http.NewRequest(method, srv.URL+path, nil)
		rq.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(rq)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := req("POST", "/h/close?session=sid1", "spy"); got != http.StatusNotFound {
		t.Fatalf("outsider /h/close: %d, want 404", got)
	}
	if got := req("POST", "/h/up?session=sid1", "spy"); got != http.StatusNotFound {
		t.Fatalf("outsider /h/up: %d, want 404", got)
	}
	// The session must survive the outsider's close attempt.
	r.hmu.Lock()
	_, alive := r.hsess["sid1"]
	r.hmu.Unlock()
	if !alive {
		t.Fatal("an outsider closed a session it was not party to")
	}
	// A real party (the owner) may close it.
	if got := req("POST", "/h/close?session=sid1", "own"); got != http.StatusOK {
		t.Fatalf("owner /h/close: %d, want 200", got)
	}
}

// The sweeper reaps a device that stopped polling and a session both parties
// abandoned, but never a fresh one (audit 2026-08-28, SEC-A-01/03).
func TestReapHTTPDropsStaleOnly(t *testing.T) {
	r := New(EnvTokenStore("t:ns"))
	now := time.Now()
	r.hagents["ns/live"] = &httpAgent{ns: "ns", device: "live", lastSeen: now}
	r.hagents["ns/dead"] = &httpAgent{ns: "ns", device: "dead", lastSeen: now.Add(-2 * httpAgentTTL)}
	r.hsess["fresh"] = &httpSession{toClient: newSideQueue(), toAgent: newSideQueue(), lastActive: now}
	r.hsess["orphan"] = &httpSession{toClient: newSideQueue(), toAgent: newSideQueue(), lastActive: now.Add(-2 * httpSessionIdle)}

	r.reapHTTP(now)

	r.hmu.Lock()
	defer r.hmu.Unlock()
	if _, ok := r.hagents["ns/live"]; !ok {
		t.Error("reaped a live agent")
	}
	if _, ok := r.hagents["ns/dead"]; ok {
		t.Error("kept a dead agent")
	}
	if _, ok := r.hsess["fresh"]; !ok {
		t.Error("reaped a fresh session")
	}
	if _, ok := r.hsess["orphan"]; ok {
		t.Error("kept an orphaned session")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}
