// Package mcp wires the wanctl-flavored Model Context Protocol server. It
// supports two shapes:
//   - ServeStdio: a child process spawned by an AI host on the user's machine,
//     backed by the local wanctl config dir.
//   - Handler / ServeHTTP: a Streamable HTTP server (multi-user) where each
//     MCP client session has its own ephemeral state and the controller
//     identity is HKDF-derived per namespace — so reconnects keep a stable
//     fingerprint with no private-key persistence.
package mcp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"wanctl/internal/client"
	"wanctl/internal/config"
	"wanctl/internal/policy"
	"wanctl/internal/transport"

	mcpapi "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"golang.org/x/crypto/hkdf"
)

// ServeStdio runs the MCP server over stdio: single user, backed by the local
// wanctl config dir. Intended for AI hosts that spawn `wanctl mcp` as a child.
func ServeStdio() error {
	sessions = &sessionStore{stdio: &localFsSession{}}
	s := server.NewMCPServer("wanctl", "1.0.0")
	registerMCPTools(s)
	return server.ServeStdio(s)
}

// Handler returns an http.Handler that serves Streamable HTTP MCP at whatever
// path you mount it under (e.g. /mcp on the relay). seed must be ≥32 bytes; it
// is used as the master secret for HKDF-deriving each namespace's controller
// identity (so the same user reconnecting keeps a stable fingerprint without
// the server ever persisting a private key).
//
// `endpointPath` is the public URL path the AI host POSTs to (usually "/mcp").
// mcp-go uses it to rewrite session URLs in responses; pass the same value you
// register the handler at.
func Handler(seed []byte, endpointPath string) (http.Handler, error) {
	if len(seed) < 32 {
		return nil, fmt.Errorf("mcp seed must be at least 32 bytes, got %d", len(seed))
	}
	sessions = &sessionStore{seed: append([]byte(nil), seed...), m: map[string]*remoteSession{}}
	go sessions.gcLoop()
	s := server.NewMCPServer("wanctl", "1.0.0")
	registerMCPTools(s)
	opts := []server.StreamableHTTPOption{}
	if endpointPath != "" {
		opts = append(opts, server.WithEndpointPath(endpointPath))
	}
	return server.NewStreamableHTTPServer(s, opts...), nil
}

// ServeHTTP is a convenience for `wanctl mcp --http :addr` that builds a
// standalone HTTP server on addr and mounts the MCP handler at /mcp.
func ServeHTTP(addr string, seed []byte) error {
	h, err := Handler(seed, "/mcp")
	if err != nil {
		return err
	}
	fmt.Printf("wanctl mcp listening on %s (Streamable HTTP); endpoint = /mcp\n", addr)
	mux := http.NewServeMux()
	mux.Handle("/mcp", h)
	mux.Handle("/mcp/", h)
	return (&http.Server{Addr: addr, Handler: mux}).ListenAndServe()
}

// --- session abstraction ---

// sessionAPI is the per-client backing the MCP tools talk to. Two impls:
//   - localFsSession (stdio): persists to wanctl's local config dir
//   - remoteSession (http): in-memory state + namespace-derived identity
type sessionAPI interface {
	client() (*client.Client, *mcpapi.CallToolResult)
	saveLogin(token, namespace string) error
	clearLogin() error
	info() string // for wanctl_status
	relayURL() string
}

type sessionStore struct {
	mu sync.Mutex

	// stdio mode
	stdio *localFsSession

	// http mode
	seed []byte
	m    map[string]*remoteSession
}

var sessions *sessionStore

func (s *sessionStore) get(ctx context.Context) sessionAPI {
	if s.stdio != nil {
		return s.stdio
	}
	sid := "default"
	if cs := server.ClientSessionFromContext(ctx); cs != nil {
		sid = cs.SessionID()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.m[sid]
	if r == nil {
		r = &remoteSession{id: sid, seed: s.seed, known: transport.NewMemStore(), lastUsed: time.Now()}
		s.m[sid] = r
	}
	r.lastUsed = time.Now()
	return r
}

// gcLoop prunes idle HTTP sessions every minute (TTL 1h). Cheap because state
// is small and re-login is just one user click.
func (s *sessionStore) gcLoop() {
	const ttl = time.Hour
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-ttl)
		s.mu.Lock()
		for id, r := range s.m {
			if r.lastUsed.Before(cutoff) {
				delete(s.m, id)
			}
		}
		s.mu.Unlock()
	}
}

// --- stdio session (delegates to local config dir, mirrors CLI behavior) ---

type localFsSession struct{}

func (l *localFsSession) client() (*client.Client, *mcpapi.CallToolResult) {
	c, err := client.New()
	if err == nil {
		return c, nil
	}
	if errors.Is(err, client.ErrNoToken) {
		return nil, loginRequired()
	}
	return nil, mcpapi.NewToolResultError(err.Error())
}

func (l *localFsSession) saveLogin(token, _ string) error { return config.SaveToken(token) }
func (l *localFsSession) clearLogin() error               { return config.ClearToken() }
func (l *localFsSession) relayURL() string {
	return strings.TrimRight(config.EnvOr("WANCTL_RELAY", config.DefaultRelay), "/")
}

func (l *localFsSession) info() string {
	tokPath, _ := config.TokenPath()
	dir, _ := transport.ConfigDir()
	envTok := os.Getenv("WANCTL_TOKEN")
	stored := config.StoredToken()
	id, _ := transport.LoadOrCreateIdentity()

	status := "NOT logged in — call wanctl_login() to start"
	switch {
	case envTok != "":
		status = "logged in (credential from WANCTL_TOKEN env)"
	case stored != "":
		status = "logged in (credential from " + tokPath + ")"
	}
	out := "mode:                  stdio (single-user, local config)\n"
	out += "status:                " + status + "\n"
	if id != nil {
		out += "controller fingerprint: " + id.Fingerprint + "\n"
	}
	out += "relay:                 " + l.relayURL() + "\n"
	out += "portal:                " + config.EnvOr("WANCTL_PORTAL", config.DefaultPortal) + "\n"
	out += "config dir:            " + dir + "  (override with WANCTL_CONFIG_DIR for per-AI-user isolation)\n"
	return out
}

// --- remote (HTTP) session: per-Mcp-Session-Id, ephemeral, identity derived ---

type remoteSession struct {
	id        string
	seed      []byte
	mu        sync.Mutex
	token     string
	namespace string
	identity  *transport.Identity
	known     *transport.Store
	lastUsed  time.Time
}

// deriveIdentity returns an Ed25519 cert deterministic for (server seed,
// namespace). Multiple sessions for the same namespace share the same
// fingerprint — pair once per device, trust holds across reconnects.
func (r *remoteSession) ensureIdentity() error {
	if r.identity != nil {
		return nil
	}
	h := hkdf.New(sha256.New, r.seed, nil, []byte("wanctl-mcp:"+r.namespace))
	out := make([]byte, ed25519.SeedSize)
	if _, err := io.ReadFull(h, out); err != nil {
		return err
	}
	id, err := transport.IdentityFromSeed(out, "wanctl-mcp:"+r.namespace)
	if err != nil {
		return err
	}
	r.identity = id
	return nil
}

func (r *remoteSession) client() (*client.Client, *mcpapi.CallToolResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.token == "" {
		return nil, loginRequired()
	}
	if err := r.ensureIdentity(); err != nil {
		return nil, mcpapi.NewToolResultError("derive identity: " + err.Error())
	}
	relay := strings.TrimRight(config.EnvOr("WANCTL_RELAY", config.DefaultRelay), "/")
	tr := config.EnvOr("WANCTL_TRANSPORT", config.DefaultTransport)
	return client.NewWith(r.identity, r.known, relay, r.token, tr), nil
}

func (r *remoteSession) saveLogin(token, namespace string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.token, r.namespace, r.identity = token, namespace, nil // force re-derive
	return r.ensureIdentity()
}

func (r *remoteSession) clearLogin() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.token, r.namespace, r.identity = "", "", nil
	r.known = transport.NewMemStore()
	return nil
}

func (r *remoteSession) relayURL() string {
	return strings.TrimRight(config.EnvOr("WANCTL_RELAY", config.DefaultRelay), "/")
}

func (r *remoteSession) info() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := "mode:                  http (remote, per-MCP-session state)\n"
	out += "session id:            " + r.id + "\n"
	if r.token == "" {
		out += "status:                NOT logged in — call wanctl_login() to start\n"
	} else {
		out += "status:                logged in to namespace \"" + r.namespace + "\"\n"
		if r.identity != nil {
			out += "controller fingerprint: " + r.identity.Fingerprint + "\n"
		}
	}
	out += "relay:                 " + r.relayURL() + "\n"
	out += "portal:                " + config.EnvOr("WANCTL_PORTAL", config.DefaultPortal) + "\n"
	out += "note:                  identity is derived per-namespace, so the same person reconnecting keeps the same fingerprint (no re-pairing).\n"
	return out
}

// --- tool registration (shared between stdio and http modes) ---

func registerMCPTools(s *server.MCPServer) {
	s.AddTool(mcpapi.NewTool("wanctl_login",
		mcpapi.WithDescription("Authenticate THIS MCP session to a wanctl namespace via the team portal. Two-step OAuth flow: (1) call with NO argument first → returns a portal URL + a one-time code prompt the user needs to complete in their browser. (2) call again with the `code` the user pastes back → exchanges it for a namespace token bound ONLY to this MCP session (in HTTP mode) or this machine's wanctl config (in stdio mode). Multiple AI users sharing the same MCP server each log in independently — credentials are never shared across sessions."),
		mcpapi.WithString("code", mcpapi.Description("The one-time code the user copied from the portal /enroll page. Omit on the first call.")),
	), mcpLogin)

	s.AddTool(mcpapi.NewTool("wanctl_status",
		mcpapi.WithDescription("Report whether this MCP session is logged in, what namespace it's bound to, and the controller fingerprint. Call this if a tool says 'login required' and you're not sure if a login already completed."),
	), mcpStatus)

	s.AddTool(mcpapi.NewTool("wanctl_logout",
		mcpapi.WithDescription("Clear this MCP session's stored credentials. Subsequent data tools (peers/exec/push/pull/logs) will require a fresh wanctl_login."),
	), mcpLogout)

	s.AddTool(mcpapi.NewTool("wanctl_peers",
		mcpapi.WithDescription("List devices currently reachable by the active controller token. Returns one device name per line. Use this FIRST when the user asks 'what devices are available' or before guessing a target name."),
	), mcpPeers)

	s.AddTool(mcpapi.NewTool("wanctl_exec",
		mcpapi.WithDescription("Run a shell command on a remote wanctl-enrolled device over the encrypted relay. Returns the device's stdout, stderr, and exit code. If the device hasn't paired this controller yet, the result is isError=true with a 'PAIRING REQUIRED' message that carries a URL — surface that URL VERBATIM to the user; do not paraphrase."),
		mcpapi.WithString("target", mcpapi.Required(), mcpapi.Description("Device name (DEVICE) or NS/DEVICE for shared devices. If exactly one device is online for this token, you may pass empty string.")),
		mcpapi.WithString("command", mcpapi.Required(), mcpapi.Description("Shell command to run, in the device's default shell (sh on Unix, powershell on Windows).")),
		mcpapi.WithString("cwd", mcpapi.Description("Working directory on the device for this command (also the policy scope).")),
		mcpapi.WithBoolean("oneshot", mcpapi.Description("Run in a fresh shell with no persistent session state. Default false — successive exec calls share cwd/env like a real terminal.")),
	), mcpExec)

	s.AddTool(mcpapi.NewTool("wanctl_push",
		mcpapi.WithDescription("Upload a local file to a remote path on the target device. Same pairing/policy rules as wanctl_exec. NOTE: in HTTP (remote) MCP mode, 'local' is a path on the MCP SERVER, not on the AI host — this tool is intended for stdio mode where the AI host has direct filesystem access."),
		mcpapi.WithString("target", mcpapi.Required(), mcpapi.Description("Device name (DEVICE) or NS/DEVICE.")),
		mcpapi.WithString("local", mcpapi.Required(), mcpapi.Description("Absolute path on the MCP-server machine (or your local machine in stdio mode) to upload.")),
		mcpapi.WithString("remote", mcpapi.Required(), mcpapi.Description("Absolute path on the target device to write to.")),
	), mcpPush)

	s.AddTool(mcpapi.NewTool("wanctl_pull",
		mcpapi.WithDescription("Download a remote file from the target device to a local path. Same pairing/policy rules as wanctl_exec. NOTE: in HTTP (remote) MCP mode 'local' is on the MCP SERVER; for AI-host-side files use stdio mode."),
		mcpapi.WithString("target", mcpapi.Required(), mcpapi.Description("Device name (DEVICE) or NS/DEVICE.")),
		mcpapi.WithString("remote", mcpapi.Required(), mcpapi.Description("Absolute path on the target device to read.")),
		mcpapi.WithString("local", mcpapi.Required(), mcpapi.Description("Absolute path on the MCP-server machine (or your local machine in stdio mode) to write to.")),
	), mcpPull)

	s.AddTool(mcpapi.NewTool("wanctl_logs",
		mcpapi.WithDescription("Pull JSONL activity events from the target device's local log (every connect/exec/file with its decision and exit code). Useful for auditing what happened, including past pairing/approval outcomes."),
		mcpapi.WithString("target", mcpapi.Required(), mcpapi.Description("Device name (DEVICE) or NS/DEVICE.")),
		mcpapi.WithString("type", mcpapi.Description("Filter: 'connect', 'exec', or 'file'.")),
		mcpapi.WithString("grep", mcpapi.Description("Filter: substring of the detail field.")),
		mcpapi.WithString("since", mcpapi.Description("Filter: RFC3339 timestamp lower bound.")),
		mcpapi.WithNumber("limit", mcpapi.Description("Return at most this many of the most recent matching events (0 = no cap).")),
	), mcpLogs)

	s.AddTool(mcpapi.NewTool("wanctl_id",
		mcpapi.WithDescription("Show THIS MCP session's controller identity fingerprint. The fingerprint is what target devices pair against in the trust step."),
	), mcpID)

	s.AddTool(mcpapi.NewTool("wanctl_trust",
		mcpapi.WithDescription("List the trust store for THIS MCP session. 'servers' (default) = devices this controller has TOFU-pinned. 'clients' = controllers this machine has trusted to drive it (only meaningful in stdio mode if this machine is also running wanctl agent)."),
		mcpapi.WithString("which", mcpapi.Description("'servers' (default) or 'clients'.")),
	), mcpTrust)

	s.AddTool(mcpapi.NewTool("wanctl_rules",
		mcpapi.WithDescription("List the local policy rules (allow-list) on THIS machine. Only meaningful in stdio mode if this machine is also running wanctl agent; for controller-only and HTTP-mode hosts the list is empty."),
	), mcpRules)
}

// --- helpers ---

func reqStr(req mcpapi.CallToolRequest, key, def string) string {
	if v, ok := req.GetArguments()[key].(string); ok {
		return v
	}
	return def
}

func reqBool(req mcpapi.CallToolRequest, key string) bool {
	if v, ok := req.GetArguments()[key].(bool); ok {
		return v
	}
	return false
}

func reqInt(req mcpapi.CallToolRequest, key string) int {
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

func asPairing(err error) *client.RejectError {
	var rej *client.RejectError
	if errors.As(err, &rej) && rej.PairingURL != "" {
		return rej
	}
	return nil
}

func pairingResult(rej *client.RejectError) *mcpapi.CallToolResult {
	return mcpapi.NewToolResultError(fmt.Sprintf(
		"PAIRING REQUIRED. This controller is not yet trusted on the target device.\n\nGive this URL to the user VERBATIM (do not shorten, paraphrase, or wrap):\n\n%s\n\nAsk them to open it, click 「信任并继续」, then retry your previous tool call. URL is valid for 5 minutes. Reason: %s",
		rej.PairingURL, rej.Reason,
	))
}

func loginRequired() *mcpapi.CallToolResult {
	return mcpapi.NewToolResultError(
		"LOGIN REQUIRED. This MCP session has no wanctl credentials yet. Call wanctl_login() (no args) — it returns a URL and instructions you should show the user. The user opens the URL, signs in via Feishu, copies the one-time code; then call wanctl_login(code=\"…\") with what they paste back. Once logged in, retry your previous tool call.",
	)
}

// --- auth tools ---

func mcpLogin(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	s := sessions.get(ctx)
	code := reqStr(req, "code", "")
	portal := config.EnvOr("WANCTL_PORTAL", config.DefaultPortal)
	if code == "" {
		return mcpapi.NewToolResultText(fmt.Sprintf(
			"OK — drive the user through Feishu SSO to mint a session token. Show them these instructions VERBATIM:\n\n"+
				"  1. Open: %s/enroll\n"+
				"  2. Sign in via Feishu (likely already logged in for the team portal).\n"+
				"  3. Copy the big one-time code shown on that page (e.g. ABCD-1234).\n"+
				"  4. Paste it back to me.\n\n"+
				"When they paste the code, call wanctl_login again with code=\"…\" to complete the login.",
			portal,
		)), nil
	}
	token, ns, err := client.ExchangeCode(ctx, s.relayURL(), code)
	if err != nil {
		return mcpapi.NewToolResultError(fmt.Sprintf("授权失败: %s\n请让用户回到 %s/enroll 拿一个新 code（旧的可能用过或过期了），然后再调一次 wanctl_login(code=\"…\")。", err, portal)), nil
	}
	if err := s.saveLogin(token, ns); err != nil {
		return mcpapi.NewToolResultError(fmt.Sprintf("保存登录态失败: %s", err)), nil
	}
	return mcpapi.NewToolResultText(fmt.Sprintf(
		"✓ 已绑定到 namespace \"%s\". 现在可以调 wanctl_peers / wanctl_exec / wanctl_push / wanctl_pull / wanctl_logs 了。\n第一次拨某台设备会返回 'PAIRING REQUIRED' + 链接,这是正常的,把链接原封不动给用户点。",
		ns,
	)), nil
}

func mcpStatus(ctx context.Context, _ mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	return mcpapi.NewToolResultText(sessions.get(ctx).info()), nil
}

func mcpLogout(ctx context.Context, _ mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	if err := sessions.get(ctx).clearLogin(); err != nil {
		return mcpapi.NewToolResultError(err.Error()), nil
	}
	return mcpapi.NewToolResultText("✓ 已清除本会话凭证. 想再操作请先 wanctl_login()."), nil
}

// --- data tools ---

func mcpPeers(ctx context.Context, _ mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	c, hint := sessions.get(ctx).client()
	if hint != nil {
		return hint, nil
	}
	devs, err := c.Peers(ctx)
	if err != nil {
		return mcpapi.NewToolResultError(err.Error()), nil
	}
	if len(devs) == 0 {
		return mcpapi.NewToolResultText("no devices online for this token"), nil
	}
	out := "online devices:\n"
	for _, d := range devs {
		out += "  " + d + "\n"
	}
	return mcpapi.NewToolResultText(out), nil
}

func mcpExec(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	target := reqStr(req, "target", "")
	command := reqStr(req, "command", "")
	if command == "" {
		return mcpapi.NewToolResultError("command is required"), nil
	}
	c, hint := sessions.get(ctx).client()
	if hint != nil {
		return hint, nil
	}
	var stdout, stderr bytes.Buffer
	code, err := c.ExecTo(ctx, target, command, reqBool(req, "oneshot"), reqStr(req, "cwd", ""), &stdout, &stderr)
	if rej := asPairing(err); rej != nil {
		return pairingResult(rej), nil
	}
	if err != nil {
		return mcpapi.NewToolResultError(err.Error()), nil
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
	return mcpapi.NewToolResultText(out), nil
}

func mcpPush(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	target := reqStr(req, "target", "")
	local := reqStr(req, "local", "")
	remote := reqStr(req, "remote", "")
	if local == "" || remote == "" {
		return mcpapi.NewToolResultError("local and remote are required"), nil
	}
	c, hint := sessions.get(ctx).client()
	if hint != nil {
		return hint, nil
	}
	if err := c.Push(ctx, target, local, remote); err != nil {
		if rej := asPairing(err); rej != nil {
			return pairingResult(rej), nil
		}
		return mcpapi.NewToolResultError(err.Error()), nil
	}
	return mcpapi.NewToolResultText(fmt.Sprintf("uploaded %s -> %s:%s", local, target, remote)), nil
}

func mcpPull(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	target := reqStr(req, "target", "")
	remote := reqStr(req, "remote", "")
	local := reqStr(req, "local", "")
	if local == "" || remote == "" {
		return mcpapi.NewToolResultError("remote and local are required"), nil
	}
	c, hint := sessions.get(ctx).client()
	if hint != nil {
		return hint, nil
	}
	if err := c.Pull(ctx, target, remote, local); err != nil {
		if rej := asPairing(err); rej != nil {
			return pairingResult(rej), nil
		}
		return mcpapi.NewToolResultError(err.Error()), nil
	}
	return mcpapi.NewToolResultText(fmt.Sprintf("downloaded %s:%s -> %s", target, remote, local)), nil
}

func mcpLogs(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	target := reqStr(req, "target", "")
	if target == "" {
		return mcpapi.NewToolResultError("target is required"), nil
	}
	c, hint := sessions.get(ctx).client()
	if hint != nil {
		return hint, nil
	}
	var buf bytes.Buffer
	if err := c.LogsTo(ctx, target, reqStr(req, "type", ""), reqStr(req, "grep", ""), reqStr(req, "since", ""), reqInt(req, "limit"), &buf); err != nil {
		if rej := asPairing(err); rej != nil {
			return pairingResult(rej), nil
		}
		return mcpapi.NewToolResultError(err.Error()), nil
	}
	if buf.Len() == 0 {
		return mcpapi.NewToolResultText("(no matching events)"), nil
	}
	return mcpapi.NewToolResultText(buf.String()), nil
}

// --- info tools (use the session's identity if any) ---

func mcpID(ctx context.Context, _ mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	s := sessions.get(ctx)
	if r, ok := s.(*remoteSession); ok {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.identity == nil {
			return mcpapi.NewToolResultText("not logged in — call wanctl_login first; identity is derived from your namespace once you do."), nil
		}
		return mcpapi.NewToolResultText(fmt.Sprintf("fingerprint: %s\nnamespace:   %s", r.identity.Fingerprint, r.namespace)), nil
	}
	// stdio: local config
	id, err := transport.LoadOrCreateIdentity()
	if err != nil {
		return mcpapi.NewToolResultError(err.Error()), nil
	}
	dir, _ := transport.ConfigDir()
	return mcpapi.NewToolResultText(fmt.Sprintf("fingerprint: %s\nconfig dir:  %s", id.Fingerprint, dir)), nil
}

func mcpTrust(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	which := reqStr(req, "which", "servers")
	if r, ok := sessions.get(ctx).(*remoteSession); ok {
		// In HTTP mode there is no on-disk trust file. List in-memory known peers.
		r.mu.Lock()
		defer r.mu.Unlock()
		peers := r.known.List()
		if len(peers) == 0 {
			return mcpapi.NewToolResultText("known servers (this MCP session, memory-only): (none)"), nil
		}
		out := fmt.Sprintf("known servers (this MCP session, memory-only): %d\n", len(peers))
		for _, p := range peers {
			out += fmt.Sprintf("  %-20s %s\n", p.Name, transport.ShortFingerprint(p.Fingerprint))
		}
		return mcpapi.NewToolResultText(out), nil
	}
	file, label := "known_servers.json", "trusted devices (TOFU-pinned)"
	if which == "clients" {
		file, label = "known_clients.json", "trusted controllers (this machine as agent)"
	}
	store, err := transport.OpenStore(file)
	if err != nil {
		return mcpapi.NewToolResultError(err.Error()), nil
	}
	peers := store.List()
	if len(peers) == 0 {
		return mcpapi.NewToolResultText(label + ": (none)"), nil
	}
	out := fmt.Sprintf("%s: %d\n", label, len(peers))
	for _, p := range peers {
		out += fmt.Sprintf("  %-20s %s  (added %s)\n", p.Name, transport.ShortFingerprint(p.Fingerprint), p.Added.Format("2006-01-02"))
	}
	return mcpapi.NewToolResultText(out), nil
}

func mcpRules(ctx context.Context, _ mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	if _, ok := sessions.get(ctx).(*remoteSession); ok {
		return mcpapi.NewToolResultText("policy rules: (none — HTTP MCP mode has no local agent policies)"), nil
	}
	eng, err := policy.Open("rules.json", policy.ModeNormal)
	if err != nil {
		return mcpapi.NewToolResultError(err.Error()), nil
	}
	rules := eng.List()
	if len(rules) == 0 {
		return mcpapi.NewToolResultText("policy rules: (none — this machine is controller-only, or no rules added)"), nil
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
	return mcpapi.NewToolResultText(out), nil
}

func endsNewline(b []byte) bool { return len(b) > 0 && b[len(b)-1] == '\n' }

// Compile-time anchor for indirectly-used packages.
var _ = http.StatusOK
