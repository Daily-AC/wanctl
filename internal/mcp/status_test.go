package mcp

import (
	"strings"
	"testing"
	"time"
)

// A hosted relay serves the MCP endpoint in-process and dials itself over
// loopback, so WANCTL_RELAY there is an address the caller of wanctl_status
// can never reach. The status line must name the origin they did reach.
func TestHostedStatusShowsPublicOrigin(t *testing.T) {
	h := newOAuthHandler(t, &oauthProbe{live: true})
	t.Setenv("WANCTL_RELAY", "http://127.0.0.1:8080")
	t.Setenv("WANCTL_PUBLIC_ORIGIN", "https://wanctl-relay.z10.dev/")
	access := bearer(t, "alice", "tok-alice", "client-1", time.Hour)
	sid := openSession(t, h, access)

	status := callTool(t, h, access, sid, "wanctl_status")
	if !strings.Contains(status, "relay:                 https://wanctl-relay.z10.dev\n") {
		t.Errorf("relay line does not show the public origin (trailing slash trimmed):\n%s", status)
	}
	if strings.Contains(status, "127.0.0.1") {
		t.Errorf("status leaks the loopback address the relay dials itself on:\n%s", status)
	}
}

// With no public origin configured, the configured relay is both what an
// operator set and what the caller can reach, so nothing changes.
func TestHostedStatusFallsBackToConfiguredRelay(t *testing.T) {
	h := newOAuthHandler(t, &oauthProbe{live: true})
	t.Setenv("WANCTL_PUBLIC_ORIGIN", "")
	access := bearer(t, "alice", "tok-alice", "client-1", time.Hour)
	sid := openSession(t, h, access)

	status := callTool(t, h, access, sid, "wanctl_status")
	if !strings.Contains(status, "relay:                 https://relay.example\n") {
		t.Errorf("relay line does not fall back to the configured relay:\n%s", status)
	}
}
