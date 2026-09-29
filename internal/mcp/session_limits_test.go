package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

// initializeWith opens a session the way a client does, with a bearer.
func initializeWith(t *testing.T, h http.Handler, access string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(initializeBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+access)
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

// Each session is state on a server every tenant shares, and a connector may
// open one per tool call. However many are asked for, the number open at once
// is bounded.
func TestOpenSessionsHaveACeiling(t *testing.T) {
	previous := sessions
	t.Cleanup(func() { sessions = previous })
	h := newOAuthHandler(t, &oauthProbe{live: true})
	access := bearer(t, "alice", "token", "chat", time.Hour)
	for i := 0; i < maxHostedSessions; i++ {
		if rr := initializeWith(t, h, access); rr.Code != http.StatusOK {
			t.Fatalf("session %d: %d %.200s", i+1, rr.Code, rr.Body.String())
		}
	}
	wantRefusal(t, initializeWith(t, h, access), http.StatusServiceUnavailable)
}

// Sessions end by going quiet far more often than by DELETE. One closes after
// sessionIdle without a request; its login is the bearer's, so the next
// request with that bearer, in whatever session, is logged in again. Closing a
// session frees what mcp-go held for it as well as this package's state.
func TestIdleSessionsAreClosedAndTheirStateFreed(t *testing.T) {
	previous := sessions
	t.Cleanup(func() { sessions = previous })
	h := newOAuthHandler(t, &oauthProbe{live: true})
	now := time.Now()
	sessions.clock = func() time.Time { return now }

	access := bearer(t, "alice", "token", "chat", time.Hour)
	first := openSession(t, h, access)
	callTool(t, h, access, first, "wanctl_status")
	// Enough idle sessions for what mcp-go keeps per session to show on the heap.
	const idle = 500
	for i := 0; i < idle; i++ {
		if rr := initializeWith(t, h, access); rr.Code != http.StatusOK {
			t.Fatalf("session %d: %d", i+1, rr.Code)
		}
	}
	var full, swept runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&full)

	now = now.Add(sessionIdle + time.Minute)
	sessions.sweep()
	runtime.GC()
	runtime.ReadMemStats(&swept)
	sessions.mu.Lock()
	openNow, statesNow := len(sessions.open), len(sessions.m)
	sessions.mu.Unlock()
	if openNow != 0 || statesNow != 0 {
		t.Fatalf("after %v idle: %d sessions open, %d states kept; want none", sessionIdle+time.Minute, openNow, statesNow)
	}
	freed := int64(full.HeapAlloc) - int64(swept.HeapAlloc)
	if freed < idle*4<<10 {
		t.Fatalf("closing %d idle sessions freed %d KiB; mcp-go's state for them was kept", idle, freed>>10)
	}
	t.Logf("closing %d idle sessions freed %d KiB", idle, freed>>10)

	// The bearer carries its login to whatever session it next arrives in,
	// including the closed one's ID, which is taken back rather than refused.
	for _, sid := range []string{first, openSession(t, h, access)} {
		if text := callTool(t, h, access, sid, "wanctl_status"); !strings.Contains(text, `logged in to namespace "alice"`) {
			t.Fatalf("bearer on session %s after the sweep: %s", sid, text)
		}
	}
}

// A client that ends its session with DELETE takes this package's record of
// it along.
func TestDeletedSessionIsForgotten(t *testing.T) {
	previous := sessions
	t.Cleanup(func() { sessions = previous })
	h := newOAuthHandler(t, &oauthProbe{live: true})
	access := bearer(t, "alice", "token", "chat", time.Hour)
	sid := openSession(t, h, access)
	callTool(t, h, access, sid, "wanctl_status")
	req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	req.Header.Set("Mcp-Session-Id", sid)
	req.Header.Set("Authorization", "Bearer "+access)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE = %d", rr.Code)
	}
	sessions.mu.Lock()
	_, open := sessions.open[sid]
	sessions.mu.Unlock()
	if open {
		t.Fatal("after DELETE the session is still open")
	}
}
