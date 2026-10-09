package client

import (
	"context"
	"testing"
	"time"

	"wanctl/internal/agent"
	"wanctl/internal/protocol"
)

func awaitBusy(t *testing.T, ag *agent.Agent, want bool, why string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for ag.Busy() != want {
		if time.Now().After(deadline) {
			t.Fatal(why)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The auto-updater restarts the agent only when Busy is false. Nothing reaps
// workspaces, so counting an open one as busy kept a device that had once used
// `wanctl workspace` off every later release (found 2026-10-02). Only a command
// in a workspace, or a controller connected over a reusable workspace link,
// holds the update back.
func TestIdleWorkspaceDoesNotBlockSelfUpdate(t *testing.T) {
	for _, tr := range []string{"ws", "http"} {
		t.Run(tr, func(t *testing.T) {
			c, ag := workspaceFixture(t, tr)
			ref := openTestWorkspace(t, c, t.TempDir())
			if ag.Busy() {
				t.Fatal("an idle open workspace blocks self-update")
			}
			if _, err := c.Workspace(context.Background(), ref, "exec", protocol.Message{RequestID: "slow", Command: "sleep 1"}); err != nil {
				t.Fatal(err)
			}
			if !ag.Busy() {
				t.Fatal("a command running in a workspace does not count as busy")
			}
			awaitWorkspace(t, c, ref, "slow")
			if ag.Busy() {
				t.Fatal("the workspace stays busy after its command finished")
			}

			link := NewWorkspaceLink()
			defer link.Close()
			c.UseWorkspaceLink(link)
			if _, err := c.Workspace(context.Background(), ref, "status", protocol.Message{}); err != nil {
				t.Fatal(err)
			}
			awaitBusy(t, ag, true, "a controller connected over a workspace link does not count as busy")
			link.Drop()
			awaitBusy(t, ag, false, "still busy after the workspace link disconnected")
		})
	}
}
