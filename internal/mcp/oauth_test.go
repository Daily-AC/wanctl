package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wanctl/internal/mcpauth"
)

const testSeed = "seed-for-mcp-oauth-tests-0123456789abcdef"

type oauthProbe struct {
	live    bool
	revoked []string
}

func newOAuthHandler(t *testing.T, probe *oauthProbe) http.Handler {
	t.Helper()
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", "https://relay.example")
	t.Setenv("WANCTL_PORTAL", "https://portal.example")
	h, err := HandlerWithOptions(Options{
		Seed:         []byte(testSeed),
		EndpointPath: "/mcp",
		OAuth: &OAuthConfig{
			ResourceMetadataURL: "https://relay.example/.well-known/oauth-protected-resource",
			Live:                func(string, string) bool { return probe.live },
			Revoke: func(ns, token string) error {
				probe.revoked = append(probe.revoked, ns+"/"+token)
				return nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func bearer(t *testing.T, namespace, token, clientID string, ttl time.Duration) string {
	t.Helper()
	access, _, err := mcpauth.SealAccess([]byte(testSeed), namespace, token, clientID, time.Now(), ttl)
	if err != nil {
		t.Fatal(err)
	}
	return access
}

// rpc posts one JSON-RPC message the way a Streamable HTTP client does and
// returns the session id the server assigned plus the decoded result.
func rpc(t *testing.T, h http.Handler, access, sessionID string, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if access != "" {
		req.Header.Set("Authorization", "Bearer "+access)
	}
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr, decodeRPC(t, rr)
}

// decodeRPC reads either a plain JSON body or one SSE frame, whichever mcp-go
// chose for this response.
func decodeRPC(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	payload := strings.TrimSpace(rr.Body.String())
	if payload == "" {
		return nil
	}
	if strings.Contains(payload, "data: ") {
		for _, line := range strings.Split(payload, "\n") {
			if after, ok := strings.CutPrefix(strings.TrimSpace(line), "data: "); ok {
				payload = after
				break
			}
		}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		return nil
	}
	return out
}

const initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{` +
	`"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`

// openSession does the handshake a client does at the start of every session.
func openSession(t *testing.T, h http.Handler, access string) string {
	t.Helper()
	rr, _ := rpc(t, h, access, "", initializeBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("initialize: %d %s", rr.Code, rr.Body.String())
	}
	sid := rr.Header().Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("initialize returned no Mcp-Session-Id")
	}
	return sid
}

func callTool(t *testing.T, h http.Handler, access, sessionID, name string) string {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"` + name + `","arguments":{}}}`
	rr, out := rpc(t, h, access, sessionID, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("tools/call %s: %d %s", name, rr.Code, rr.Body.String())
	}
	result, _ := out["result"].(map[string]any)
	content, _ := result["content"].([]any)
	var text strings.Builder
	for _, c := range content {
		if m, ok := c.(map[string]any); ok {
			if s, ok := m["text"].(string); ok {
				text.WriteString(s)
			}
		}
	}
	if text.Len() == 0 {
		t.Fatalf("tools/call %s returned no text: %s", name, rr.Body.String())
	}
	return text.String()
}

// This is the case the whole feature exists for. ChatGPT's MCP client opens a
// fresh session for every tool call: it re-sends initialize and gets a new
// Mcp-Session-Id each time. Under the session-keyed login that meant the call
// right after a successful wanctl_login reported LOGIN REQUIRED. With a bearer,
// both sessions are the same authenticated person.
func TestOneBearerIsLoggedInAcrossTwoSessions(t *testing.T) {
	h := newOAuthHandler(t, &oauthProbe{live: true})
	access := bearer(t, "alice", "tok-alice", "client-1", time.Hour)

	var sessionIDs []string
	for i := 0; i < 2; i++ {
		sid := openSession(t, h, access)
		sessionIDs = append(sessionIDs, sid)
		status := callTool(t, h, access, sid, "wanctl_status")
		if !strings.Contains(status, `logged in to namespace "alice"`) {
			t.Fatalf("session %d reports: %s", i, status)
		}
		if !strings.Contains(status, "OAuth bearer") {
			t.Errorf("session %d does not say how it is authenticated: %s", i, status)
		}
	}
	if sessionIDs[0] == sessionIDs[1] {
		t.Fatalf("both calls landed on one MCP session (%s); the test is not exercising the bug it guards",
			sessionIDs[0])
	}
}

// Two sessions on one bearer must also be one session's worth of state, or a
// device pinned in the first would be unknown in the second.
func TestBearerSessionsShareStateAndTrust(t *testing.T) {
	h := newOAuthHandler(t, &oauthProbe{live: true})
	access := bearer(t, "alice", "tok-alice", "client-1", time.Hour)
	first, second := openSession(t, h, access), openSession(t, h, access)
	callTool(t, h, access, first, "wanctl_status")
	callTool(t, h, access, second, "wanctl_status")

	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	if got := len(sessions.m); got != 1 {
		t.Fatalf("two MCP sessions on one bearer produced %d wanctl sessions, want 1", got)
	}
	// And a second namespace must not be handed the first one's pinned servers.
	if sessions.trustForLocked("alice") == sessions.trustForLocked("bob") {
		t.Error("two namespaces share one trust store")
	}
	if sessions.trustForLocked("alice") != sessions.trustForLocked("alice") {
		t.Error("the same namespace got two trust stores")
	}
}

// An expired or revoked bearer has to fail as HTTP 401 with the challenge, not
// as a tool-level error: 401 plus resource_metadata is what makes a client
// refresh or re-authorize on its own.
func TestInvalidBearerAnswers401WithTheChallenge(t *testing.T) {
	for name, tc := range map[string]struct {
		live  bool
		token string
	}{
		"expired": {live: true, token: ""},
		"revoked": {live: false, token: ""},
		"garbage": {live: true, token: "woa1.not-a-real-envelope"},
		"foreign": {live: true, token: "some-other-products-token"},
	} {
		probe := &oauthProbe{live: tc.live}
		h := newOAuthHandler(t, probe)
		access := tc.token
		if access == "" {
			ttl := time.Hour
			if name == "expired" {
				ttl = -time.Minute
			}
			access = bearer(t, "alice", "tok-alice", "client-1", ttl)
		}
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(initializeBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+access)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401 (%s)", name, rr.Code, rr.Body.String())
			continue
		}
		challenge := rr.Header().Get("WWW-Authenticate")
		if !strings.Contains(challenge, `resource_metadata="https://relay.example/.well-known/oauth-protected-resource"`) {
			t.Errorf("%s: WWW-Authenticate = %q", name, challenge)
		}
	}
}

// The old path is the one Claude Code, Codex and Cursor are on. Turning OAuth
// on must not change it: no Authorization header, no bearer, same per-session
// login as before.
func TestNoBearerKeepsThePerSessionPath(t *testing.T) {
	h := newOAuthHandler(t, &oauthProbe{live: true})
	sid := openSession(t, h, "")
	status := callTool(t, h, "", sid, "wanctl_status")
	if !strings.Contains(status, "NOT logged in") {
		t.Fatalf("a session with no bearer should start logged out: %s", status)
	}
	if strings.Contains(status, "OAuth") {
		t.Errorf("a session with no bearer should not mention OAuth: %s", status)
	}
	// And wanctl_login still opens the portal-code flow rather than being short
	// circuited by the OAuth branch.
	login := callTool(t, h, "", sid, "wanctl_login")
	if !strings.Contains(login, "/enroll") {
		t.Errorf("wanctl_login on the session path = %s", login)
	}
}

func TestLoginOnTheBearerPathSaysItIsAlreadyDone(t *testing.T) {
	h := newOAuthHandler(t, &oauthProbe{live: true})
	access := bearer(t, "alice", "tok-alice", "client-1", time.Hour)
	sid := openSession(t, h, access)
	login := callTool(t, h, access, sid, "wanctl_login")
	// It must not hand back the portal URL the code flow prints: that would
	// send someone who is already signed in round the browser a second time.
	if !strings.Contains(login, "alice") || strings.Contains(login, "https://portal.example/enroll") {
		t.Fatalf("wanctl_login on the bearer path = %s", login)
	}
}

// Logout has to reach the relay. Forgetting the token in this process would
// change nothing: the next request carries the bearer and rebuilds the session.
func TestLogoutOnTheBearerPathRevokesTheRelayToken(t *testing.T) {
	probe := &oauthProbe{live: true}
	h := newOAuthHandler(t, probe)
	access := bearer(t, "alice", "tok-alice", "client-1", time.Hour)
	sid := openSession(t, h, access)
	out := callTool(t, h, access, sid, "wanctl_logout")
	if !strings.Contains(out, "吊销") {
		t.Fatalf("logout said: %s", out)
	}
	if len(probe.revoked) != 1 || probe.revoked[0] != "alice/tok-alice" {
		t.Fatalf("revoked = %v, want one entry for alice/tok-alice", probe.revoked)
	}
}

// A bearer for one namespace must never open another's session, whatever the
// MCP session id says.
func TestBearersForDifferentNamespacesDoNotShareASession(t *testing.T) {
	h := newOAuthHandler(t, &oauthProbe{live: true})
	alice := bearer(t, "alice", "tok-alice", "client-1", time.Hour)
	bob := bearer(t, "bob", "tok-bob", "client-1", time.Hour)
	sid := openSession(t, h, alice)
	// Same MCP session id, different bearer: the session the tools see follows
	// the bearer, not the id.
	if got := callTool(t, h, bob, sid, "wanctl_status"); !strings.Contains(got, `namespace "bob"`) {
		t.Fatalf("bob's bearer on alice's session id reports: %s", got)
	}
	if got := callTool(t, h, alice, sid, "wanctl_status"); !strings.Contains(got, `namespace "alice"`) {
		t.Fatalf("alice's bearer reports: %s", got)
	}
}

// Without OAuth configured the gate is not installed at all, so an Authorization
// header a proxy happened to add cannot make the endpoint start answering 401.
func TestHandlerWithoutOAuthIgnoresAuthorization(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", "https://relay.example")
	h, err := Handler([]byte(testSeed), "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(initializeBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer whatever")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rr.Code, rr.Body.String())
	}
}
