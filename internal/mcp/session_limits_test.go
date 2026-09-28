package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

func hostedHandler(t *testing.T) http.Handler {
	t.Helper()
	previous := sessions
	t.Cleanup(func() { sessions = previous })
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", "https://relay.example")
	h, err := Handler([]byte(testSeed), "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// initializeFrom opens a session the way a client does, arriving from remote.
func initializeFrom(t *testing.T, h http.Handler, remote string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(initializeBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.RemoteAddr = remote
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// wantRefusal checks a refused initialize is an HTTP error a client can show,
// carrying a JSON-RPC error that says why, and no session.
func wantRefusal(t *testing.T, rr *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rr.Code != status {
		t.Fatalf("status = %d, want %d: %.200s", rr.Code, status, rr.Body.String())
	}
	if sid := rr.Header().Get("Mcp-Session-Id"); sid != "" {
		t.Fatalf("a refused initialize was given session %s", sid)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || !strings.Contains(body.Error.Message, "too many") {
		t.Fatalf("refusal body = %s, want a JSON-RPC error saying why", rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("refusal carries no Retry-After")
	}
}

// Anyone can open a session without logging in, and each one is state on a
// server every tenant shares. One address gets a bounded number of sessions
// nobody has logged in to; the next address is unaffected.
func TestSessionsWithoutLoginAreBoundedPerClient(t *testing.T) {
	h := hostedHandler(t)
	for i := 0; i < maxAnonymousSessionsPerClient; i++ {
		if rr := initializeFrom(t, h, "203.0.113.5:4000"); rr.Code != http.StatusOK || rr.Header().Get("Mcp-Session-Id") == "" {
			t.Fatalf("session %d from one client: %d %.200s", i+1, rr.Code, rr.Body.String())
		}
	}
	wantRefusal(t, initializeFrom(t, h, "203.0.113.5:4001"), http.StatusTooManyRequests)
	if rr := initializeFrom(t, h, "203.0.113.6:4000"); rr.Code != http.StatusOK {
		t.Fatalf("a session from another client: %d %.200s", rr.Code, rr.Body.String())
	}
}

// However many addresses ask, the number of open sessions is bounded.
func TestOpenSessionsHaveACeiling(t *testing.T) {
	h := hostedHandler(t)
	for i := 0; i < maxHostedSessions; i++ {
		remote := fmt.Sprintf("10.%d.%d.1:4000", i/maxAnonymousSessionsPerClient/256, i/maxAnonymousSessionsPerClient%256)
		if rr := initializeFrom(t, h, remote); rr.Code != http.StatusOK {
			t.Fatalf("session %d: %d %.200s", i+1, rr.Code, rr.Body.String())
		}
	}
	wantRefusal(t, initializeFrom(t, h, "198.51.100.99:4000"), http.StatusServiceUnavailable)
}

// markLoggedIn gives a session the login wanctl_login would, without a relay
// to exchange a code with.
func markLoggedIn(t *testing.T, h http.Handler, sid string) {
	t.Helper()
	callTool(t, h, "", sid, "wanctl_status") // brings the session's state into being
	sessions.mu.Lock()
	r := sessions.m[sid]
	sessions.mu.Unlock()
	r.mu.Lock()
	r.token, r.namespace = "wanctl_test_token", "alice"
	r.mu.Unlock()
}

// A session that has logged in no longer counts against its address's share of
// sessions nobody has logged in to.
func TestLoggedInSessionsLeaveTheAnonymousShare(t *testing.T) {
	h := hostedHandler(t)
	var first string
	for i := 0; i < maxAnonymousSessionsPerClient; i++ {
		rr := initializeFrom(t, h, "203.0.113.5:4000")
		if rr.Code != http.StatusOK {
			t.Fatalf("session %d: %d", i+1, rr.Code)
		}
		if first == "" {
			first = rr.Header().Get("Mcp-Session-Id")
		}
	}
	wantRefusal(t, initializeFrom(t, h, "203.0.113.5:4000"), http.StatusTooManyRequests)
	markLoggedIn(t, h, first)
	if rr := initializeFrom(t, h, "203.0.113.5:4000"); rr.Code != http.StatusOK {
		t.Fatalf("after one session logged in, a new one = %d %.200s", rr.Code, rr.Body.String())
	}
}

// Sessions end by going quiet far more often than by DELETE. One nobody logged
// in to closes after anonymousSessionIdle without a request, and so does one
// opened with a bearer, whose login is the bearer's; a session logged in with
// wanctl_login keeps its login for loggedInSessionIdle. Closing a session frees
// what mcp-go held for it as well as this package's state.
func TestIdleSessionsAreClosedAndTheirStateFreed(t *testing.T) {
	previous := sessions
	t.Cleanup(func() { sessions = previous })
	h := newOAuthHandler(t, &oauthProbe{live: true})
	now := time.Now()
	sessions.clock = func() time.Time { return now }

	anonymous := openSession(t, h, "")
	loggedIn := openSession(t, h, "")
	markLoggedIn(t, h, loggedIn)
	access := bearer(t, "alice", "token", "chat", time.Hour)
	withBearer := openSession(t, h, access)
	callTool(t, h, access, withBearer, "wanctl_status")
	// Enough idle sessions for what mcp-go keeps per session to show on the heap.
	const idle = 500
	for i := 0; i < idle; i++ {
		if rr := initializeFrom(t, h, fmt.Sprintf("10.2.%d.%d:1", i/200, i%200)); rr.Code != http.StatusOK {
			t.Fatalf("session %d: %d", i+1, rr.Code)
		}
	}
	var full, swept runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&full)

	now = now.Add(anonymousSessionIdle + time.Minute)
	sessions.sweep()
	runtime.GC()
	runtime.ReadMemStats(&swept)
	sessions.mu.Lock()
	_, loggedInOpen := sessions.open[loggedIn]
	_, anonymousOpen := sessions.open[anonymous]
	_, bearerOpen := sessions.open[withBearer]
	openNow, statesNow := len(sessions.open), len(sessions.m)
	sessions.mu.Unlock()
	if !loggedInOpen || anonymousOpen || bearerOpen || openNow != 1 {
		t.Fatalf("after %v idle: logged-in open=%v, anonymous open=%v, bearer open=%v, %d open in all; want only the logged-in one",
			anonymousSessionIdle+time.Minute, loggedInOpen, anonymousOpen, bearerOpen, openNow)
	}
	if statesNow != 2 { // the logged-in session's, and the bearer's own
		t.Fatalf("%d session states remain, want 2", statesNow)
	}
	freed := int64(full.HeapAlloc) - int64(swept.HeapAlloc)
	if freed < idle*4<<10 {
		t.Fatalf("closing %d idle sessions freed %d KiB; mcp-go's state for them was kept", idle, freed>>10)
	}
	t.Logf("closing %d idle sessions freed %d KiB", idle, freed>>10)

	// The bearer carries its login to whatever session it next arrives in.
	if text := callTool(t, h, access, openSession(t, h, access), "wanctl_status"); !strings.Contains(text, "logged in to namespace \"alice\"") {
		t.Fatalf("bearer after its session closed: %s", text)
	}

	now = now.Add(loggedInSessionIdle)
	sessions.sweep()
	sessions.mu.Lock()
	openNow, statesNow = len(sessions.open), len(sessions.m)
	sessions.mu.Unlock()
	if openNow != 0 || statesNow != 0 {
		t.Fatalf("after %v more: %d sessions open, %d states kept; want none", loggedInSessionIdle, openNow, statesNow)
	}

	// A client that carries on with a closed session's ID finds it again,
	// logged out — the same as after a relay restart, which rebind repairs.
	if text := callTool(t, h, "", loggedIn, "wanctl_status"); !strings.Contains(text, "NOT logged in") {
		t.Fatalf("a closed session's ID: %s", text)
	}
}

// A client that ends its session with DELETE takes this package's state for
// it along.
func TestDeletedSessionIsForgotten(t *testing.T) {
	h := hostedHandler(t)
	sid := openSession(t, h, "")
	callTool(t, h, "", sid, "wanctl_status")
	req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	req.Header.Set("Mcp-Session-Id", sid)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE = %d", rr.Code)
	}
	sessions.mu.Lock()
	_, open := sessions.open[sid]
	_, state := sessions.m[sid]
	sessions.mu.Unlock()
	if open || state {
		t.Fatalf("after DELETE: open=%v state=%v", open, state)
	}
}
