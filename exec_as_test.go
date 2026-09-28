package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"wanctl/internal/client"
	"wanctl/internal/protocol"
)

func TestExecAsRejectsElevationFlags(t *testing.T) {
	for _, flag := range []string{"--elevate", "--via=adb"} {
		err := cmdExec(context.Background(), []string{"--as", "alice", flag, "whoami"})
		if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
			t.Fatalf("--as %s: %v", flag, err)
		}
	}
}

func TestWorkspaceExecRejectsAsBeforeSendingScript(t *testing.T) {
	_, err := execWorkspace(context.Background(), nil, client.WorkspaceRef{}, protocol.Message{As: "alice"}, "nonexistent.ps1", "", false, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--as") {
		t.Fatalf("workspace --as: %v", err)
	}
}
