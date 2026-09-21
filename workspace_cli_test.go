package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wanctl/internal/agent"
	"wanctl/internal/client"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
	"wanctl/internal/relay"
	"wanctl/internal/transport"
)

// Every CLI invocation is a separate OS process, talking through a real relay,
// TLS and device shell. This is the local harness lifetime we must support.
func TestWorkspaceCLIThroughSeparateProcesses(t *testing.T) {
	bin := buildWanctl(t)
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("workspace-cli:alice")).Handler())
	defer srv.Close()
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	ag, err := agent.New(agent.Options{RelayURL: srv.URL, Token: "workspace-cli", Name: "cli-device", AutoYes: true, Mode: policy.ModeBypass, Transport: "http"})
	if err != nil {
		t.Fatal(err)
	}
	deviceID, err := transport.LoadOrCreateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	defer ag.Close()
	go ag.Run(ctx)
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", srv.URL)
	t.Setenv("WANCTL_TOKEN", "workspace-cli")
	t.Setenv("WANCTL_TRANSPORT", "http")
	t.Setenv("WANCTL_WORKSPACE", "")
	c, err := client.New()
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		peers, e := c.Peers(ctx)
		if e == nil && len(peers) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("registration: %v", e)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := c.PinServer(ctx, "alice/"+ag.DeviceID(), deviceID.Fingerprint, false); err != nil {
		t.Fatal(err)
	}
	run := func(want int, ref string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = append(os.Environ(), "WANCTL_WORKSPACE="+ref)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		code := 0
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatal(err)
			}
			code = exit.ExitCode()
		}
		if code != want {
			t.Fatalf("%v exited %d, want %d\nstdout: %s\nstderr: %s", args, code, want, &stdout, &stderr)
		}
		return stdout.String()
	}
	decode := func(out string) (string, protocol.WorkspaceResult) {
		t.Helper()
		var result struct {
			Workspace string `json:"workspace"`
			protocol.WorkspaceResult
		}
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Fatalf("JSON: %v: %s", err, out)
		}
		return result.Workspace, result.WorkspaceResult
	}
	root := t.TempDir()
	a, _ := decode(run(0, "", "workspace", "enter", "--target", "cli-device", "--root", root))
	b, _ := decode(run(0, "", "workspace", "enter", "--target", "cli-device", "--root", t.TempDir()))
	run(0, a, "exec", "--request-id", "setup", "mkdir child; cd child; export WORKSPACE_LESSON=retained")
	got := run(0, a, "exec", "printf '%s\\n' \"$WORKSPACE_LESSON\"; pwd")
	if !strings.Contains(got, "retained\n") || !strings.Contains(got, "/child") {
		t.Fatalf("state lost across processes: %q", got)
	}
	if got := run(0, b, "exec", "printf '%s' \"${WORKSPACE_LESSON-unset}\""); strings.TrimSpace(got) != "unset" {
		t.Fatalf("cross-workspace leak: %q", got)
	}
	source := filepath.Join(t.TempDir(), "state.sh")
	if err := os.WriteFile(source, []byte("cd ..\nexport WORKSPACE_LESSON=script\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run(0, a, "exec", "--script", source)
	if got := run(0, a, "exec", "printf '%s' \"$WORKSPACE_LESSON\""); strings.TrimSpace(got) != "script" {
		t.Fatalf("script lost state: %q", got)
	}
	run(0, a, "write", "note.txt", "--content", "before\n")
	run(0, a, "edit", "note.txt", "--old", "before", "--new", "after")
	if got := run(0, a, "read", "note.txt"); got != "after\n" {
		t.Fatalf("file output polluted: %q", got)
	}
	if got, err := os.ReadFile(filepath.Join(root, "note.txt")); err != nil || string(got) != "after\n" {
		t.Fatalf("wrong physical file: %q, %v", got, err)
	}
	run(7, a, "exec", "sh -c 'exit 7'")
	// More than one retained page must be drained even after done=true.
	if got := run(0, a, "exec", "head -c 120000 /dev/zero | tr '\\000' x"); len(strings.TrimSuffix(got, "\n")) != 120000 {
		t.Fatalf("lost output pages: %d bytes", len(got))
	}
	run(0, a, "exec", "--request-id", "once", "printf x >> once.txt")
	run(0, a, "exec", "--request-id", "once", "printf x >> once.txt")
	if got := run(0, a, "read", "once.txt"); got != "x" {
		t.Fatalf("request executed twice: %q", got)
	}
	run(1, a, "exec", "--request-id", "once", "printf y >> once.txt")
	_, job := decode(run(0, a, "exec", "--async", "--request-id", "detached", "sleep 0.2; printf finished"))
	for !job.Done {
		_, job = decode(run(0, a, "workspace", "poll", "--request-id", "detached"))
	}
	if strings.TrimSpace(job.Output) != "finished" || job.Code != 0 {
		t.Fatalf("async result: %+v", job)
	}
	// A new MCP process for EACH tool call inherits the CLI reference. It must
	// use the same shell without needing a previous in-process enter/attach.
	for i, command := range []string{"export WORKSPACE_LESSON=from_mcp", "printf '%s' \"$WORKSPACE_LESSON\""} {
		cmd := exec.CommandContext(ctx, bin, "mcp", "--workspace-session")
		cmd.Env = append(os.Environ(), "WANCTL_WORKSPACE="+a)
		input, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		output, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		send := func(request any) map[string]any {
			t.Helper()
			if err := json.NewEncoder(input).Encode(request); err != nil {
				t.Fatal(err)
			}
			if !scanner.Scan() {
				t.Fatalf("MCP ended: %v", scanner.Err())
			}
			var response map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			return response
		}
		send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "cli-trial", "version": "1"}}})
		if err := json.NewEncoder(input).Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); err != nil {
			t.Fatal(err)
		}
		response := send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "wanctl_exec", "arguments": map[string]any{"command": command}}})
		input.Close()
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
		result, ok := response["result"].(map[string]any)
		if !ok || result["isError"] == true {
			t.Fatalf("MCP inherited workspace: %v", response)
		}
		_, state := decode(result["content"].([]any)[0].(map[string]any)["text"].(string))
		if !state.Done || state.Code != 0 || (i == 1 && strings.TrimSpace(state.Output) != "from_mcp") {
			t.Fatalf("MCP restarted workspace: %+v", state)
		}
	}
	if got := run(0, a, "exec", "printf '%s' \"$WORKSPACE_LESSON\""); strings.TrimSpace(got) != "from_mcp" {
		t.Fatalf("CLI cannot resume MCP state: %q", got)
	}
	run(0, "", "workspace", "attach", "--workspace", a)
	run(1, a, "exec", "--target", "cli-device", "echo wrong-route")
	run(1, a, "exec", "--oneshot", "echo wrong-shell")
	run(1, "invalid-reference", "read", "note.txt")
	run(0, b, "exec", "--async", "--request-id", "cancel-me", "sleep 30")
	_, stopped := decode(run(0, b, "workspace", "cancel", "--request-id", "cancel-me"))
	if stopped.State != "invalid" || !stopped.Done {
		t.Fatalf("cancel: %+v", stopped)
	}
	run(1, b, "exec", "echo cannot-recreate")
	run(0, b, "workspace", "exit")
	run(0, a, "workspace", "exit")
	run(1, a, "read", "note.txt")
	run(1, a, "exec", "echo cannot-fall-back")
}
