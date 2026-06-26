package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"wanctl/internal/client"
	"wanctl/internal/config"
	"wanctl/internal/eventlog"
	"wanctl/internal/policy"
	"wanctl/internal/transport"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// cmdMCP runs a Model Context Protocol server on stdio so an AI host (Claude
// Code, Cursor, Continue, …) can drive `wanctl` natively as typed tools rather
// than shelling out through Bash. Token / relay / transport come from the same
// env + stored-credential lookup the CLI uses (`wanctl login` works first).
//
// AI hosts register it once:
//
//	claude mcp add wanctl wanctl mcp
//
// Tools mirror the controller-side CLI: peers / exec / push / pull / logs +
// id / trust / rules (local info that helps the AI debug its own state). When
// the device hasn't paired this controller yet, the tool returns isError=true
// with a "PAIRING REQUIRED" message that carries the one-click URL verbatim,
// so the AI can hand it to its user without paraphrasing.
func cmdMCP(ctx context.Context) error {
	s := server.NewMCPServer("wanctl", "1.0.0")
	registerMCPTools(s)
	return server.ServeStdio(s)
}

func registerMCPTools(s *server.MCPServer) {
	s.AddTool(mcp.NewTool("wanctl_peers",
		mcp.WithDescription("List devices currently reachable by the active controller token. Returns one device name per line, plus a hint about which is online if known. Use this FIRST when the user asks 'what devices are available' or before guessing a target name."),
	), mcpPeers)

	s.AddTool(mcp.NewTool("wanctl_exec",
		mcp.WithDescription("Run a shell command on a remote wanctl-enrolled device over the encrypted relay. Returns the device's stdout, stderr, and exit code. If the device rejects with a pairing URL, surface that URL VERBATIM to the user; do not paraphrase."),
		mcp.WithString("target", mcp.Required(), mcp.Description("Device name (DEVICE) or NS/DEVICE for shared devices. If exactly one device is online for this token, you may pass empty string.")),
		mcp.WithString("command", mcp.Required(), mcp.Description("Shell command to run, in the device's default shell (sh on Unix, powershell on Windows).")),
		mcp.WithString("cwd", mcp.Description("Working directory on the device for this command (also the policy scope).")),
		mcp.WithBoolean("oneshot", mcp.Description("Run in a fresh shell with no persistent session state. Default false — successive exec calls share cwd/env like a real terminal.")),
	), mcpExec)

	s.AddTool(mcp.NewTool("wanctl_push",
		mcp.WithDescription("Upload a local file to a remote path on the target device. Same pairing/policy rules as wanctl_exec."),
		mcp.WithString("target", mcp.Required(), mcp.Description("Device name (DEVICE) or NS/DEVICE.")),
		mcp.WithString("local", mcp.Required(), mcp.Description("Absolute path on THIS machine to upload.")),
		mcp.WithString("remote", mcp.Required(), mcp.Description("Absolute path on the target device to write to.")),
	), mcpPush)

	s.AddTool(mcp.NewTool("wanctl_pull",
		mcp.WithDescription("Download a remote file from the target device to a local path. Same pairing/policy rules as wanctl_exec."),
		mcp.WithString("target", mcp.Required(), mcp.Description("Device name (DEVICE) or NS/DEVICE.")),
		mcp.WithString("remote", mcp.Required(), mcp.Description("Absolute path on the target device to read.")),
		mcp.WithString("local", mcp.Required(), mcp.Description("Absolute path on THIS machine to write to.")),
	), mcpPull)

	s.AddTool(mcp.NewTool("wanctl_logs",
		mcp.WithDescription("Pull JSONL activity events from the target device's local log (every connect/exec/file with its decision and exit code). Useful for auditing what happened, including past pairing/approval outcomes."),
		mcp.WithString("target", mcp.Required(), mcp.Description("Device name (DEVICE) or NS/DEVICE.")),
		mcp.WithString("type", mcp.Description("Filter: 'connect', 'exec', or 'file'.")),
		mcp.WithString("grep", mcp.Description("Filter: substring of the detail field.")),
		mcp.WithString("since", mcp.Description("Filter: RFC3339 timestamp lower bound.")),
		mcp.WithNumber("limit", mcp.Description("Return at most this many of the most recent matching events (0 = no cap).")),
	), mcpLogs)

	s.AddTool(mcp.NewTool("wanctl_id",
		mcp.WithDescription("Show THIS controller's identity fingerprint and config dir. The fingerprint is what target devices pair against in the trust step."),
	), mcpID)

	s.AddTool(mcp.NewTool("wanctl_trust",
		mcp.WithDescription("List the trust store on THIS machine. 'servers' (default) = devices this controller has TOFU-pinned. 'clients' = controllers this machine has trusted to drive it (only meaningful if this machine is also running wanctl agent)."),
		mcp.WithString("which", mcp.Description("'servers' (default) or 'clients'.")),
	), mcpTrust)

	s.AddTool(mcp.NewTool("wanctl_rules",
		mcp.WithDescription("List the local policy rules (allow-list) on THIS machine. Only meaningful if this machine is also running wanctl agent; for controller-only hosts the list is empty."),
	), mcpRules)
}

// --- helpers ---

// reqStr extracts a string argument with optional fallback.
func reqStr(req mcp.CallToolRequest, key, def string) string {
	if v, ok := req.GetArguments()[key].(string); ok {
		return v
	}
	return def
}

func reqBool(req mcp.CallToolRequest, key string) bool {
	if v, ok := req.GetArguments()[key].(bool); ok {
		return v
	}
	return false
}

func reqInt(req mcp.CallToolRequest, key string) int {
	switch v := req.GetArguments()[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

// asPairing extracts a structured pairing-required reject so the MCP handler
// can return a clear isError=true result that contains the URL verbatim.
func asPairing(err error) *client.RejectError {
	var rej *client.RejectError
	if errors.As(err, &rej) && rej.PairingURL != "" {
		return rej
	}
	return nil
}

func pairingResult(rej *client.RejectError) *mcp.CallToolResult {
	return mcp.NewToolResultError(fmt.Sprintf(
		"PAIRING REQUIRED. This controller is not yet trusted on the target device.\n\nGive this URL to the user VERBATIM (do not shorten, paraphrase, or wrap):\n\n%s\n\nAsk them to open it, click 「信任并继续」, then retry your previous tool call. URL is valid for 5 minutes. Reason: %s",
		rej.PairingURL, rej.Reason,
	))
}

// --- tool handlers ---

func mcpPeers(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	c, err := client.New()
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	devs, err := c.Peers(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if len(devs) == 0 {
		return mcp.NewToolResultText("no devices online for this token"), nil
	}
	out := "online devices:\n"
	for _, d := range devs {
		out += "  " + d + "\n"
	}
	return mcp.NewToolResultText(out), nil
}

func mcpExec(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	target := reqStr(req, "target", "")
	command := reqStr(req, "command", "")
	if command == "" {
		return mcp.NewToolResultError("command is required"), nil
	}
	c, err := client.New()
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var stdout, stderr bytes.Buffer
	code, err := c.ExecTo(ctx, target, command, reqBool(req, "oneshot"), reqStr(req, "cwd", ""), &stdout, &stderr)
	if rej := asPairing(err); rej != nil {
		return pairingResult(rej), nil
	}
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	out := fmt.Sprintf("exit: %d\n", code)
	if stdout.Len() > 0 {
		out += "\n--- stdout ---\n" + stdout.String()
		if !endsNewline(stdout.Bytes()) {
			out += "\n"
		}
	}
	if stderr.Len() > 0 {
		out += "\n--- stderr ---\n" + stderr.String()
		if !endsNewline(stderr.Bytes()) {
			out += "\n"
		}
	}
	if stdout.Len() == 0 && stderr.Len() == 0 {
		out += "(no output)\n"
	}
	return mcp.NewToolResultText(out), nil
}

func mcpPush(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	target := reqStr(req, "target", "")
	local := reqStr(req, "local", "")
	remote := reqStr(req, "remote", "")
	if local == "" || remote == "" {
		return mcp.NewToolResultError("local and remote are required"), nil
	}
	c, err := client.New()
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := c.Push(ctx, target, local, remote); err != nil {
		if rej := asPairing(err); rej != nil {
			return pairingResult(rej), nil
		}
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("uploaded %s -> %s:%s", local, target, remote)), nil
}

func mcpPull(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	target := reqStr(req, "target", "")
	remote := reqStr(req, "remote", "")
	local := reqStr(req, "local", "")
	if local == "" || remote == "" {
		return mcp.NewToolResultError("remote and local are required"), nil
	}
	c, err := client.New()
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := c.Pull(ctx, target, remote, local); err != nil {
		if rej := asPairing(err); rej != nil {
			return pairingResult(rej), nil
		}
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("downloaded %s:%s -> %s", target, remote, local)), nil
}

func mcpLogs(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	target := reqStr(req, "target", "")
	if target == "" {
		return mcp.NewToolResultError("target is required"), nil
	}
	c, err := client.New()
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var buf bytes.Buffer
	if err := c.LogsTo(ctx, target, reqStr(req, "type", ""), reqStr(req, "grep", ""), reqStr(req, "since", ""), reqInt(req, "limit"), &buf); err != nil {
		if rej := asPairing(err); rej != nil {
			return pairingResult(rej), nil
		}
		return mcp.NewToolResultError(err.Error()), nil
	}
	if buf.Len() == 0 {
		return mcp.NewToolResultText("(no matching events)"), nil
	}
	return mcp.NewToolResultText(buf.String()), nil
}

func mcpID(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := transport.LoadOrCreateIdentity()
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	dir, _ := transport.ConfigDir()
	return mcp.NewToolResultText(fmt.Sprintf("fingerprint: %s\nconfig dir:  %s", id.Fingerprint, dir)), nil
}

func mcpTrust(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	which := reqStr(req, "which", "servers")
	file, label := "known_servers.json", "trusted devices (TOFU-pinned)"
	if which == "clients" {
		file, label = "known_clients.json", "trusted controllers (this machine as agent)"
	}
	store, err := transport.OpenStore(file)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	peers := store.List()
	if len(peers) == 0 {
		return mcp.NewToolResultText(label + ": (none)"), nil
	}
	out := fmt.Sprintf("%s: %d\n", label, len(peers))
	for _, p := range peers {
		out += fmt.Sprintf("  %-20s %s  (added %s)\n", p.Name, transport.ShortFingerprint(p.Fingerprint), p.Added.Format("2006-01-02"))
	}
	return mcp.NewToolResultText(out), nil
}

func mcpRules(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	eng, err := policy.Open("rules.json", policy.ModeNormal)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	rules := eng.List()
	if len(rules) == 0 {
		return mcp.NewToolResultText("policy rules: (none — this machine is controller-only, or no rules added)"), nil
	}
	out := fmt.Sprintf("policy rules: %d\n", len(rules))
	for i, r := range rules {
		scope := string(r.Scope)
		if r.Scope == policy.ScopeDir {
			scope = "dir:" + r.Dir
			if r.Kind != policy.KindExec {
				scope = "dir:" + r.Pattern
			}
		}
		out += fmt.Sprintf("  [%d] %-5s %-30q %s\n", i, r.Kind, r.Pattern, scope)
	}
	return mcp.NewToolResultText(out), nil
}

func endsNewline(b []byte) bool { return len(b) > 0 && b[len(b)-1] == '\n' }

// _ keep imports tidy (some only used by indirect helpers in this file).
var (
	_ = json.Marshal
	_ = time.Now
	_ = os.Stdout
	_ = config.DefaultRelay
	_ = eventlog.Event{}
)
