package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wanctl/internal/catalog"
	"wanctl/internal/client"
	"wanctl/internal/protocol"

	mcpapi "github.com/mark3labs/mcp-go/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

type wireOutputChecker map[string]*jsonschema.Schema

// Compile what tools/list actually sent, rather than validating against an
// independent test schema that could agree with the handler but not the wire.
func outputSchemasFromWire(t *testing.T, tools []any) wireOutputChecker {
	t.Helper()
	checks := wireOutputChecker{}
	for _, entry := range tools {
		tool := entry.(map[string]any)
		raw, exists := tool["outputSchema"]
		if !exists {
			continue
		}
		name := tool["name"].(string)
		compiler := jsonschema.NewCompiler()
		url := "https://wanctl.invalid/output/" + name
		if err := compiler.AddResource(url, raw); err != nil {
			t.Fatal(err)
		}
		schema, err := compiler.Compile(url)
		if err != nil {
			t.Fatalf("%s output schema: %v", name, err)
		}
		checks[name] = schema
	}
	for _, name := range []string{"wanctl_workspace", "wanctl_peers", "wanctl_exec", "wanctl_exec_async", "wanctl_exec_poll", "wanctl_read", "wanctl_write", "wanctl_edit"} {
		if checks[name] == nil {
			t.Errorf("tools/list omitted outputSchema for %s", name)
		}
	}
	return checks
}

func (checks wireOutputChecker) check(t *testing.T, name string, result map[string]any) {
	t.Helper()
	schema := checks[name]
	if schema == nil {
		return
	}
	data, exists := result["structuredContent"]
	if !exists {
		// Pre-execution failures keep the MCP diagnostic contract. A command
		// failure WITH structured data still has to describe that data truthfully.
		if result["isError"] != true {
			t.Fatalf("%s succeeded without structuredContent: %v", name, result)
		}
		return
	}
	if err := schema.Validate(data); err != nil {
		t.Fatalf("%s returned data outside its advertised schema: %v\n%v", name, err, data)
	}
}

func decodedJSON(t *testing.T, value any) map[string]any {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func registeredOutputSchemas(t *testing.T) wireOutputChecker {
	t.Helper()
	var wire []any
	for _, tool := range newMCPServer().ListTools() {
		wire = append(wire, decodedJSON(t, tool.Tool))
	}
	return outputSchemasFromWire(t, wire)
}

func TestOutputSchemasCoverLifecycleAndEmptyPeers(t *testing.T) {
	checks := registeredOutputSchemas(t)
	ref := client.WorkspaceRef{Target: "alice/device", ID: "w-test"}
	for _, state := range []string{"open", "invalid", "closed"} {
		// Lifecycle operations have no request_id, done=false and code=0.
		// Declaring done=true as a condition of success would break exit.
		result := workspaceResult(ref, &protocol.WorkspaceResult{ID: ref.ID, Root: "/project", State: state})
		checks.check(t, "wanctl_workspace", decodedJSON(t, result))
	}
	for _, peers := range []client.Peers{
		{}, // A legacy relay may send null collections and omit namespace.
		{Namespace: "alice", Devices: []string{}, Aliases: map[string]string{}},
		{Namespace: "alice", Shared: []client.SharedDevice{{Owner: "bob", Device: "device", Target: "bob/device", Online: false}}},
	} {
		checks.check(t, "wanctl_peers", decodedJSON(t, peerToolResult(peers, map[string]bool{})))
	}
	failed := workspaceResult(ref, &protocol.WorkspaceResult{ID: ref.ID, Root: "/project", State: "open", RequestID: "failure", RequestState: "done", Done: true, Code: 7, Error: "command failed"})
	checks.check(t, "wanctl_exec", decodedJSON(t, failed))
	if !failed.IsError {
		t.Fatal("workspace failure lost its MCP isError flag")
	}
}

func TestOutputSchemaRejectsAmbiguousFieldTypes(t *testing.T) {
	checks := registeredOutputSchemas(t)
	for _, c := range catalog.MCPCommands() {
		if checks[c.MCPName] == nil {
			continue
		}
		if err := checks[c.MCPName].Validate(map[string]any{}); err == nil {
			t.Errorf("%s accepts an empty success object", c.MCPName)
		}
	}
	bad := map[string]any{"path": "file", "created": "false", "size_bytes": float64(0), "sha256": "hash"}
	if err := checks["wanctl_write"].Validate(bad); err == nil {
		t.Fatal("string 'false' accepted where the agent needs a boolean")
	}
	bad["created"], bad["size_bytes"] = false, float64(-1)
	if err := checks["wanctl_write"].Validate(bad); err == nil {
		t.Fatal("negative file size accepted")
	}
}

func TestServerRejectsNonconformingStructuredOutput(t *testing.T) {
	s := newMCPServer()
	tool := s.ListTools()["wanctl_write"].Tool
	s.AddTool(tool, func(context.Context, mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
		return structuredResult("looks successful", map[string]any{"path": "file", "created": "false", "size_bytes": 0, "sha256": "hash"}), nil
	})
	reply := s.HandleMessage(t.Context(), json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wanctl_write","arguments":{"path":"file","content":""}}}`))
	result := reply.(mcpapi.JSONRPCResponse).Result.(*mcpapi.CallToolResult)
	if !result.IsError || !strings.Contains(toolText(result), "output schema validation failed") {
		t.Fatalf("invalid handler output escaped validation: %#v", result)
	}
}

// Exercise the actual registered handlers over authenticated HTTP MCP, with
// the same real agent/filesystem as the workspace integration test.
func checkOutputModes(t *testing.T, call func(string, map[string]any) (string, bool), last func() map[string]any, target, ref, root string) {
	t.Helper()
	invoke := func(name string, args map[string]any) map[string]any {
		t.Helper()
		text, bad := call(name, args)
		if bad {
			t.Fatalf("%s: %s", name, text)
		}
		return last()["structuredContent"].(map[string]any)
	}
	invoke("wanctl_peers", map[string]any{})
	data := invoke("wanctl_exec", map[string]any{"target": target, "oneshot": true, "command": "printf 'out'; printf 'err' >&2; exit 7"})
	// Ordinary shell execution merges both streams on the device. The MCP
	// result describes the received channels rather than inventing separation.
	if data["code"] != float64(7) || data["stdout"] != "outerr" || data["stderr"] != "" || data["done"] != true {
		t.Fatalf("ordinary exec lost streams or exit status: %v", data)
	}
	data = invoke("wanctl_exec", map[string]any{"target": target, "oneshot": true, "command": "printf '%60000s' x"})
	if data["stdout_truncated"] != true || data["stderr_truncated"] != false {
		t.Fatalf("ordinary exec truncation: %v", data)
	}
	if spill, ok := data["spill_path"].(string); ok {
		t.Cleanup(func() { os.Remove(spill) })
	}
	// A gate makes a running poll deterministic even on a busy CI worker.
	for _, workspace := range []bool{false, true} {
		gate := filepath.Join(root, "job-release")
		_ = os.Remove(gate)
		t.Cleanup(func() { _ = os.WriteFile(gate, nil, 0600) })
		command := "while [ ! -e '" + strings.ReplaceAll(gate, "'", "'\\''") + "' ]; do sleep 0.01; done; printf background"
		args := map[string]any{"target": target, "command": command}
		if workspace {
			delete(args, "target")
			args["workspace"], args["request_id"] = ref, "schema-async"
		}
		job := invoke("wanctl_exec_async", args)
		poll := map[string]any{"target": target, "job_id": job["job_id"]}
		if workspace {
			delete(poll, "target")
			poll["workspace"], poll["job_id"] = ref, job["request_id"]
		}
		data = invoke("wanctl_exec_poll", poll)
		if data["done"] != false {
			t.Fatalf("held job already done: %v", data)
		}
		if !workspace {
			if _, hasCode := data["code"]; hasCode {
				t.Fatal("a running legacy job advertised a final exit code")
			}
		}
		if err := os.WriteFile(gate, nil, 0600); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(5 * time.Second); data["done"] != true; {
			if time.Now().After(deadline) {
				t.Fatal("async command never completed")
			}
			data = invoke("wanctl_exec_poll", poll)
		}
		if data["code"] != float64(0) || !strings.Contains(data["output"].(string), "background") {
			t.Fatalf("async result lost output or exit code: %v", data)
		}
	}
	// File metadata is useful to the next tool: a successful read's hash must
	// be usable by edit, while a stale hash must still refuse the write.
	file := filepath.Join(root, "schema-file.txt")
	data = invoke("wanctl_write", map[string]any{"target": target, "path": file, "content": ""})
	if data["created"] != true {
		t.Fatal("new file was not reported as created")
	}
	data = invoke("wanctl_read", map[string]any{"workspace": ref, "path": "schema-file.txt"})
	if data["content"] != "" || data["total_lines"] != float64(0) || data["size_bytes"] != float64(0) {
		t.Fatalf("empty file metadata: %v", data)
	}
	data = invoke("wanctl_write", map[string]any{"workspace": ref, "path": "schema-file.txt", "content": "苹果\n"})
	if data["created"] != false || data["size_bytes"] != float64(len("苹果\n")) {
		t.Fatalf("overwrite metadata: %v", data)
	}
	data = invoke("wanctl_read", map[string]any{"target": target, "path": file})
	sha := data["sha256"]
	data = invoke("wanctl_edit", map[string]any{"target": target, "path": file, "old": "苹果", "new": "桃子", "expected_sha256": sha})
	if data["replaced"] != float64(1) || data["sha256"] == sha {
		t.Fatalf("edit metadata: %v", data)
	}
	text, bad := call("wanctl_edit", map[string]any{"workspace": ref, "path": "schema-file.txt", "old": "桃子", "new": "wrong", "expected_sha256": sha})
	if !bad || !strings.Contains(text, "changed since") || last()["structuredContent"] != nil {
		t.Fatalf("stale-hash refusal became a success-shaped response: %v %s", last(), text)
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "桃子\n" {
		t.Fatalf("failed edit changed file: %q %v", b, err)
	}
	// Read pagination uses line offsets; command polling uses byte offsets.
	large := strings.Repeat("苹果\n", 60000)
	if err := os.WriteFile(file, []byte(large), 0600); err != nil {
		t.Fatal(err)
	}
	data = invoke("wanctl_read", map[string]any{"target": target, "path": file, "limit": 100000})
	if data["truncated"] != true || data["next_offset"] != data["last_line"].(float64)+1 || data["long_line"] != float64(0) {
		t.Fatalf("read pagination metadata: %v", data)
	}
	if err := os.WriteFile(file, []byte(strings.Repeat("x", 300000)), 0600); err != nil {
		t.Fatal(err)
	}
	data = invoke("wanctl_read", map[string]any{"workspace": ref, "path": "schema-file.txt"})
	if data["truncated"] != true || data["long_line"] != float64(1) {
		t.Fatal("oversized line was not reported")
	}
	if _, next := data["next_offset"]; next {
		t.Fatal("oversized line incorrectly promised lossless pagination")
	}
}
