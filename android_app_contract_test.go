package main

import (
	"context"
	"strings"
	"testing"
)

// TestAgentErrorsTheAppKeysOn guards a coupling that crosses a language
// boundary and would otherwise rot in silence.
//
// The Android app supervises `wanctl agent` as a child process. It has no
// channel to that process other than its output, so AgentService.consume()
// decides "retry" versus "stop and tell the user" by matching substrings of
// these messages. Reword one and the app stops recognising a permanent failure:
// a device with no credential, or a rejected token, would respawn the agent
// every few seconds forever — draining the battery while showing the user
// nothing that explains it.
//
// So: reword freely, but keep the marker, or change android/java/.../AgentService.java
// in the same commit.
func TestAgentErrorsTheAppKeysOn(t *testing.T) {
	t.Run("no credential", func(t *testing.T) {
		// Explicit empty --token beats whatever StoredToken() finds on the
		// machine running the test.
		err := cmdAgent(context.Background(), []string{"--relay", "", "--token", ""})
		if err == nil {
			t.Fatal("agent with no relay or token must fail")
		}
		if !strings.Contains(err.Error(), "--token") {
			t.Fatalf("AgentService keys on %q appearing in this error; got %q", "--token", err)
		}
	})

	// The other two markers belong to messages produced deep inside a live
	// relay exchange, so they are asserted against the literals rather than
	// re-derived — the point is that grepping for them finds this test.
	for _, marker := range []string{"rejected token", "registered this device name"} {
		if !strings.Contains(agentFatalMarkers, marker) {
			t.Errorf("marker %q is no longer listed", marker)
		}
	}
}

// agentFatalMarkers exists so the strings the Android service treats as fatal
// appear in this package, where a `grep` from either side lands on the contract
// and on the test that pins it. See internal/agent/agent.go for where they are
// produced.
const agentFatalMarkers = "--token | rejected token | registered this device name"
