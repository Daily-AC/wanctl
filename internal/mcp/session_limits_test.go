package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	for i := 0; i < 32; i++ {
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
	for i := 0; i < 2048; i++ {
		remote := fmt.Sprintf("10.%d.%d.1:4000", i/32/256, i/32%256)
		if rr := initializeFrom(t, h, remote); rr.Code != http.StatusOK {
			t.Fatalf("session %d: %d %.200s", i+1, rr.Code, rr.Body.String())
		}
	}
	wantRefusal(t, initializeFrom(t, h, "198.51.100.99:4000"), http.StatusServiceUnavailable)
}
