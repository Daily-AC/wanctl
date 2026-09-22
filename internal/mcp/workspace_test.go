package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wanctl/internal/agent"
	"wanctl/internal/mcpauth"
	"wanctl/internal/policy"
	"wanctl/internal/relay"
	"wanctl/internal/transport"
)

// A real HTTP MCP client, OAuth gate, relay, mutually authenticated device
// connection, shell and filesystem. A new MCP session is used for EVERY call,
// matching web hosts that cannot hold one transport session across turns.
func TestWorkspaceThroughHTTPMCPAcrossFreshSessions(t *testing.T) {
	previous := sessions
	t.Cleanup(func() { sessions = previous })
	relayServer := httptest.NewServer(relay.New(relay.EnvTokenStore("workspace-test:alice")).Handler())
	defer relayServer.Close()
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	ag, err := agent.New(agent.Options{RelayURL: relayServer.URL, Token: "workspace-test", Name: "lesson-device", AutoYes: true, Mode: policy.ModeBypass, Transport: "http"})
	if err != nil {
		t.Fatal(err)
	}
	deviceIdentity, err := transport.LoadOrCreateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer ag.Close()
	go ag.Run(ctx)
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", relayServer.URL)
	t.Setenv("WANCTL_TRANSPORT", "http")
	h, err := HandlerWithOptions(Options{Seed: []byte(testSeed), EndpointPath: "/mcp", OAuth: &OAuthConfig{
		ResourceMetadataURL: "https://example.invalid/resource",
		Live:                func(ns, token string) bool { return ns == "alice" && token == "workspace-test" },
	}})
	if err != nil {
		t.Fatal(err)
	}
	access, claim, err := mcpauth.SealAccess([]byte(testSeed), "alice", "workspace-test", "lesson", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c, hint := sessions.oauthSession(claim).client()
	if hint != nil {
		t.Fatal(toolText(hint))
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
	// The test owns both identities, so the pin comes from the agent's key,
	// independently of whatever a network response claims.
	if _, err := c.PinServer(ctx, "alice/"+ag.DeviceID(), deviceIdentity.Fingerprint, false); err != nil {
		t.Fatal(err)
	}
	endpoint := httptest.NewServer(h)
	defer endpoint.Close()
	httpClient := &http.Client{Timeout: 5 * time.Second}
	post := func(sid string, body []byte) (http.Header, map[string]any) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.URL+"/mcp", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+access)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
			req.Header.Set("MCP-Protocol-Version", "2025-06-18")
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("HTTP %d: %s", resp.StatusCode, b)
		}
		payload := string(b)
		for _, line := range strings.Split(payload, "\n") {
			if s, ok := strings.CutPrefix(line, "data: "); ok {
				payload = s
				break
			}
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			t.Fatalf("RPC decode: %v %s", err, payload)
		}
		return resp.Header, decoded
	}
	seen := map[string]bool{}
	var outputChecks wireOutputChecker
	var lastResult map[string]any
	call := func(name string, args map[string]any) (string, bool) {
		t.Helper()
		head, _ := post("", []byte(initializeBody))
		sid := head.Get("Mcp-Session-Id")
		if sid == "" || seen[sid] {
			t.Fatal("expected a fresh MCP session")
		}
		seen[sid] = true
		if outputChecks == nil {
			_, listed := post(sid, []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`))
			outputChecks = outputSchemasFromWire(t, listed["result"].(map[string]any)["tools"].([]any))
		}
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
		_, reply := post(sid, body)
		if e := reply["error"]; e != nil {
			t.Fatalf("RPC error: %v", e)
		}
		result := reply["result"].(map[string]any)
		outputChecks.check(t, name, result)
		lastResult = result
		isError, _ := result["isError"].(bool)
		var text strings.Builder
		for _, item := range result["content"].([]any) {
			if s, ok := item.(map[string]any)["text"].(string); ok {
				text.WriteString(s)
			}
		}
		return text.String(), isError
	}
	decode := func(text string) map[string]any {
		t.Helper()
		var d map[string]any
		if err := json.Unmarshal([]byte(text), &d); err != nil {
			t.Fatalf("workspace JSON: %s", text)
		}
		return d
	}
	root := t.TempDir()
	text, bad := call("wanctl_workspace", map[string]any{"action": "enter", "target": "lesson-device", "root": root})
	if bad {
		t.Fatal(text)
	}
	ref := decode(text)["workspace"].(string)
	otherText, bad := call("wanctl_workspace", map[string]any{"action": "enter", "target": "lesson-device", "root": t.TempDir()})
	if bad {
		t.Fatal(otherText)
	}
	other := decode(otherText)["workspace"].(string)
	run := func(ref, id, command string) map[string]any {
		t.Helper()
		text, bad := call("wanctl_exec", map[string]any{"workspace": ref, "request_id": id, "command": command})
		if bad {
			t.Fatal(text)
		}
		d := decode(text)
		for deadline := time.Now().Add(5 * time.Second); d["done"] != true; {
			if time.Now().After(deadline) {
				t.Fatal("workspace exec did not finish")
			}
			time.Sleep(10 * time.Millisecond)
			text, bad = call("wanctl_exec_poll", map[string]any{"workspace": ref, "job_id": id})
			if bad {
				t.Fatal(text)
			}
			d = decode(text)
		}
		return d
	}
	run(ref, "set", "export WANCTL_WORKSPACE_LESSON=retained")
	text, bad = call("wanctl_exec", map[string]any{"workspace": ref, "request_id": "script-env", "script": "export WANCTL_SCRIPT_LESSON=retained\n", "interp": "sh"})
	if bad {
		t.Fatal(text)
	}
	if decode(text)["done"] != true {
		t.Fatal("short script did not finish in the initial device response")
	}
	if got := run(ref, "script-get", "printf '%s' \"$WANCTL_SCRIPT_LESSON\"")["output"].(string); !strings.Contains(got, "retained") {
		t.Fatalf("script state lost through MCP: %q", got)
	}
	if got := run(ref, "get", "printf '%s' \"$WANCTL_WORKSPACE_LESSON\"")["output"].(string); !strings.Contains(got, "retained") {
		t.Fatalf("lost state: %q", got)
	}
	if got := run(other, "isolation", "printf '%s' \"${WANCTL_WORKSPACE_LESSON-unset}\"")["output"].(string); !strings.Contains(got, "unset") {
		t.Fatalf("shared state: %q", got)
	}
	text, bad = call("wanctl_write", map[string]any{"workspace": ref, "path": "hello.txt", "content": "hello"})
	if bad {
		t.Fatal(text)
	}
	text, bad = call("wanctl_edit", map[string]any{"workspace": ref, "path": "hello.txt", "old": "hello", "new": "workspace"})
	if bad {
		t.Fatal(text)
	}
	text, bad = call("wanctl_read", map[string]any{"workspace": ref, "path": "hello.txt"})
	if bad || !strings.Contains(text, "workspace") {
		t.Fatal(text)
	}
	if b, err := os.ReadFile(filepath.Join(root, "hello.txt")); err != nil || string(b) != "workspace" {
		t.Fatalf("actual file=%q %v", b, err)
	}
	checkOutputModes(t, call, func() map[string]any { return lastResult }, "alice/"+ag.DeviceID(), ref, root)
	text, bad = call("wanctl_exec", map[string]any{"command": "printf wrong-device"})
	if !bad {
		t.Fatalf("missing routing silently used a device: %s", text)
	}
	text, bad = call("wanctl_workspace", map[string]any{"action": "exit", "workspace": ref})
	if bad {
		t.Fatal(text)
	}
	text, bad = call("wanctl_read", map[string]any{"workspace": ref, "path": "hello.txt"})
	if !bad {
		t.Fatalf("closed workspace read: %s", text)
	}
	text, bad = call("wanctl_workspace", map[string]any{"action": "status", "workspace": other})
	if bad {
		t.Fatal(text)
	}
	t.Logf("verified %d fresh authenticated MCP sessions, two isolated workspaces, real files and persistent shell", len(seen))
	for _, bound := range []bool{false, true} {
		t.Run(map[bool]string{false: "stdio_binary", true: "stdio_conversation"}[bound], func(t *testing.T) {
			binary := filepath.Join(t.TempDir(), "wanctl")
			buildCtx, stopBuild := context.WithTimeout(ctx, 60*time.Second)
			defer stopBuild()
			if out, err := exec.CommandContext(buildCtx, "go", "build", "-o", binary, "../..").CombinedOutput(); err != nil {
				t.Fatalf("build MCP executable: %v %s", err, out)
			}
			// Stdio owns an ordinary local controller identity and pin store.
			known, err := transport.OpenStore("known_servers.json")
			if err != nil {
				t.Fatal(err)
			}
			if err := known.Pin("alice/"+ag.DeviceID(), deviceIdentity.Fingerprint, false); err != nil {
				t.Fatal(err)
			}
			t.Setenv("WANCTL_TOKEN", "workspace-test")
			stdioCtx, stopStdio := context.WithTimeout(ctx, 15*time.Second)
			defer stopStdio()
			args := []string{"mcp"}
			if bound {
				args = append(args, "--workspace-session")
			}
			cmd := exec.CommandContext(stdioCtx, binary, args...)
			input, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			output, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { input.Close(); stopStdio(); cmd.Wait() }()
			scanner := bufio.NewScanner(output)
			scanner.Buffer(make([]byte, 4096), 1<<20)
			send := func(body []byte) map[string]any {
				t.Helper()
				if _, err := input.Write(append(body, '\n')); err != nil {
					t.Fatal(err)
				}
				if !scanner.Scan() {
					t.Fatalf("stdio ended: %v", scanner.Err())
				}
				var d map[string]any
				if err := json.Unmarshal(scanner.Bytes(), &d); err != nil {
					t.Fatalf("stdio JSON: %v %s", err, scanner.Text())
				}
				if d["error"] != nil {
					t.Fatalf("stdio RPC: %v", d["error"])
				}
				return d
			}
			send([]byte(initializeBody))
			if _, err := io.WriteString(input, "{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n"); err != nil {
				t.Fatal(err)
			}
			listed := send([]byte(`{"jsonrpc":"2.0","id":99,"method":"tools/list"}`))
			stdioOutputChecks := outputSchemasFromWire(t, listed["result"].(map[string]any)["tools"].([]any))
			seq := 1
			callStdio := func(name string, args map[string]any) string {
				t.Helper()
				seq++
				body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": seq, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
				d := send(body)["result"].(map[string]any)
				stdioOutputChecks.check(t, name, d)
				text := d["content"].([]any)[0].(map[string]any)["text"].(string)
				if d["isError"] == true {
					t.Fatalf("stdio %s: %s", name, text)
				}
				return text
			}
			entered := decode(callStdio("wanctl_workspace", map[string]any{"action": "enter", "target": "lesson-device", "root": root}))
			ref := entered["workspace"].(string)
			for _, item := range []struct{ id, cmd string }{{"set", "export WANCTL_STDIO_LESSON=retained"}, {"get", "printf '%s' \"$WANCTL_STDIO_LESSON\""}} {
				args := map[string]any{"request_id": item.id, "command": item.cmd}
				if !bound {
					args["workspace"] = ref
				}
				r := decode(callStdio("wanctl_exec", args))
				for r["done"] != true {
					time.Sleep(10 * time.Millisecond)
					poll := map[string]any{"job_id": item.id}
					if !bound {
						poll["workspace"] = ref
					}
					r = decode(callStdio("wanctl_exec_poll", poll))
				}
				if item.id == "get" && !strings.Contains(r["output"].(string), "retained") {
					t.Fatalf("stdio lost shell state: %v", r)
				}
			}
			if bound {
				callStdio("wanctl_write", map[string]any{"path": "bound.txt", "content": "bound"})
				if text := callStdio("wanctl_read", map[string]any{"path": "bound.txt"}); !strings.Contains(text, "bound") {
					t.Fatal(text)
				}
				callStdio("wanctl_workspace", map[string]any{"action": "exit"})
				body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 100, "method": "tools/call", "params": map[string]any{"name": "wanctl_exec", "arguments": map[string]any{"command": "printf should-not-run"}}})
				if send(body)["result"].(map[string]any)["isError"] != true {
					t.Fatal("unbound call executed after exit")
				}
			} else {
				callStdio("wanctl_workspace", map[string]any{"action": "exit", "workspace": ref})
			}
		})
	}

}
