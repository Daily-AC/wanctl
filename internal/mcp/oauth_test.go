package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wanctl/internal/mcpauth"
)

const testSeed = "seed-for-mcp-oauth-tests-0123456789abcdef"

func TestOAuthIDBeforeAnyDeviceCall(t *testing.T) {
	h := newOAuthHandler(t, &oauthProbe{live: true})
	access := bearer(t, "alice", "token", "chat", time.Hour)
	sid := openSession(t, h, access)
	rr, result := rpc(t, h, access, sid, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"wanctl_id","arguments":{}}}`)
	body, _ := json.Marshal(result)
	if rr.Code != http.StatusOK || !strings.Contains(string(body), "fingerprint: SHA256:") || strings.Contains(string(body), "not logged in") {
		t.Fatalf("OAuth identity before first device call: %d %s", rr.Code, body)
	}
}

type oauthProbe struct {
	live    bool
	revoked []string
}

func newOAuthHandler(t *testing.T, probe *oauthProbe) http.Handler {
	t.Helper()
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", "https://relay.example")
	t.Setenv("WANCTL_PORTAL", "https://portal.example")
	h, err := Handler(Options{
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
// Mcp-Session-Id each time. Under the session-keyed login this endpoint used to
// have, the call right after a successful login reported LOGIN REQUIRED. With a
// bearer, both sessions are the same authenticated person.
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

// A request with no Authorization is where every MCP client starts. It gets
// the bare challenge (no error code: RFC 6750 §3.1), which sends the client to
// the resource metadata and into the authorization flow, and nothing else: no
// session, no instructions, no tool. Before v0.19.0 it opened a session that
// logged in with a portal code and a rebind credential whose logout lived in
// process memory (CX-04); that path is gone.
func TestNoBearerGetsTheChallengeAndNoSession(t *testing.T) {
	h := newOAuthHandler(t, &oauthProbe{live: true})
	rr, out := rpc(t, h, "", "", initializeBody)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("initialize without Authorization: %d %s", rr.Code, rr.Body.String())
	}
	want := `Bearer resource_metadata="https://relay.example/.well-known/oauth-protected-resource"`
	if got := rr.Header().Get("WWW-Authenticate"); got != want {
		t.Errorf("WWW-Authenticate = %q, want %q", got, want)
	}
	if sid := rr.Header().Get("Mcp-Session-Id"); sid != "" {
		t.Errorf("an unauthenticated initialize was given session %s", sid)
	}
	if _, ok := out["result"]; ok {
		t.Errorf("an unauthenticated initialize got a result: %s", rr.Body.String())
	}
}

// Nor can a tool call without a bearer reach a device, even on a session a
// bearer opened: the session id is not a credential.
func TestNoBearerToolCallReachesNoDevice(t *testing.T) {
	var hits atomic.Int32
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "unexpected", http.StatusTeapot)
	}))
	defer relay.Close()
	h := newOAuthHandler(t, &oauthProbe{live: true})
	t.Setenv("WANCTL_RELAY", relay.URL)
	access := bearer(t, "alice", "tok-alice", "client-1", time.Hour)
	sid := openSession(t, h, access)

	for _, session := range []string{"", sid} {
		for _, call := range []string{
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"wanctl_peers","arguments":{}}}`,
			`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"wanctl_exec","arguments":{"target":"alice/box","command":"echo hi"}}}`,
			`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"wanctl_login","arguments":{"code":"ABCD-1234"}}}`,
		} {
			rr, out := rpc(t, h, "", session, call)
			if rr.Code != http.StatusUnauthorized || out["result"] != nil {
				t.Errorf("session %q, %s: %d %s", session, call, rr.Code, rr.Body.String())
			}
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("unauthenticated tool calls reached the relay %d times", n)
	}
	// The same call with the bearer does dial out, so the count above is
	// measuring something.
	callTool(t, h, access, sid, "wanctl_peers")
	if hits.Load() == 0 {
		t.Fatal("an authenticated wanctl_peers never reached the relay; the probe is not wired")
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

// Without OAuth there is no way into the hosted endpoint, so there is no
// handler: the relay mounts Unavailable instead, which says why.
func TestHandlerNeedsOAuth(t *testing.T) {
	if _, err := Handler(Options{Seed: []byte(testSeed), EndpointPath: "/mcp"}); err == nil {
		t.Fatal("a hosted handler without OAuth was built")
	}
	rr := httptest.NewRecorder()
	Unavailable("hosted MCP is off on this relay").ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(initializeBody)))
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "hosted MCP is off") {
		t.Fatalf("Unavailable: %d %s", rr.Code, rr.Body.String())
	}
}
