package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"wanctl/internal/policy"
	"wanctl/internal/protocol"
)

func TestExecAsRefusesUnsupportedPlatformOverRelay(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exercises the non-Windows refusal")
	}
	c, ctx := startDevice(t, policy.ModeBypass)
	path := filepath.Join(t.TempDir(), "untouched")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	res, err := c.ExecOut(ctx, ExecRequest{
		Target: "home-pc", As: "alice", Command: "echo changed > '" + path + "'",
	}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "only supported by Windows") || res.Code != -1 {
		t.Fatalf("exec --as: %+v, %v, output %q", res, err, out.String())
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "original" {
		t.Fatalf("unsupported --as executed the command: %q, %v", data, err)
	}
	// Builtins must not escape the identity requirement either.
	_, err = c.ExecOut(ctx, ExecRequest{Target: "home-pc", As: "alice", Command: "help"}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "only supported by Windows") || out.Len() != 0 {
		t.Fatalf("builtin bypassed --as: output %q, error %v", out.String(), err)
	}
}

// Model the old peer's actual behavior: unknown JSON fields are discarded,
// ordinary exec is executable, and an unknown kind returns an error. A mere
// As field on KindExec would already have run the installer as SYSTEM.
func TestExecAsOldPeerRejectsBeforeExecution(t *testing.T) {
	req := ExecRequest{Target: "home-pc", As: "alice", Command: "install"}
	var wire bytes.Buffer
	if err := protocol.WriteMessage(&wire, req.message()); err != nil {
		t.Fatal(err)
	}
	_, payload, err := protocol.ReadFrame(&wire)
	if err != nil {
		t.Fatal(err)
	}
	var legacy struct {
		Kind    string `json:"kind"`
		Command string `json:"command"`
	}
	if err := json.Unmarshal(payload, &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Kind == protocol.KindExec {
		t.Fatalf("old peer would execute %q as the agent", legacy.Command)
	}
	if err := protocol.WriteMessage(&wire, protocol.Message{Kind: protocol.KindError, Reason: "unknown request: " + legacy.Kind}); err != nil {
		t.Fatal(err)
	}
	_, err = execOver(context.Background(), &wire, req, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "command was not run") || !strings.Contains(err.Error(), "update the device agent") {
		t.Fatalf("old peer error = %v", err)
	}
}

func TestExecAsValidatesBeforeConnecting(t *testing.T) {
	// A nil client proves these failures precede any attempt to dial or run.
	var c *Client
	for _, req := range []ExecRequest{{As: "alice", Elevate: true}, {As: "alice", Via: "adb"}} {
		if _, err := c.ExecOut(context.Background(), req, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
			t.Fatalf("conflicting options: %v", err)
		}
	}
	if _, err := c.Workspace(context.Background(), WorkspaceRef{}, "exec", protocol.Message{As: "alice", Command: "install"}); err == nil || !strings.Contains(err.Error(), "does not support --as") {
		t.Fatalf("workspace --as: %v", err)
	}
}
