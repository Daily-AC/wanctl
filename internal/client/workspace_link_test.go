package client

import (
	"context"
	"strings"
	"testing"

	"wanctl/internal/protocol"
)

func TestWorkspaceReusesAuthenticatedChannel(t *testing.T) {
	for _, tr := range []string{"ws", "http"} {
		t.Run(tr, func(t *testing.T) {
			c, _ := workspaceFixture(t, tr)
			link := NewWorkspaceLink()
			defer link.Close()
			c.UseWorkspaceLink(link)
			ref := openTestWorkspace(t, c, t.TempDir())
			first := link.conn
			if first == nil {
				t.Fatal("no reusable connection")
			}
			runWorkspace(t, c, ref, "export", "export WORKSPACE_REUSE=kept")
			if got := runWorkspace(t, c, ref, "read", "printf '%s' \"$WORKSPACE_REUSE\""); !strings.Contains(got.Output, "kept") {
				t.Fatal(got)
			}
			if _, err := c.WriteFile(context.Background(), WriteRequest{Target: ref.Target, WorkspaceID: ref.ID, Path: "a.txt", Content: "hello"}); err != nil {
				t.Fatal(err)
			}
			if _, err := c.EditFile(context.Background(), EditRequest{Target: ref.Target, WorkspaceID: ref.ID, Path: "a.txt", Old: "hello", New: "world"}); err != nil {
				t.Fatal(err)
			}
			got, err := c.ReadFile(context.Background(), ReadRequest{Target: ref.Target, WorkspaceID: ref.ID, Path: "a.txt"})
			if err != nil || got.Content != "world" {
				t.Fatalf("read: %+v %v", got, err)
			}
			if link.conn != first {
				t.Fatal("normal operations redialed the device")
			}
			// Local transport loss does not change the workspace or replay a write.
			link.Drop()
			if got := runWorkspace(t, c, ref, "reconnected", "printf '%s' \"$WORKSPACE_REUSE\""); !strings.Contains(got.Output, "kept") {
				t.Fatal(got)
			}
			if link.conn == first {
				t.Fatal("did not establish a fresh connection")
			}
			if _, err := c.Workspace(context.Background(), ref, "close", protocol.Message{}); err != nil {
				t.Fatal(err)
			}
			if link.conn != nil {
				t.Fatal("exit kept the connection")
			}
		})
	}
}
