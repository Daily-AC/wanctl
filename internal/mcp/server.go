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
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"wanctl/internal/client"
	"wanctl/internal/config"
	"wanctl/internal/limits"
	"wanctl/internal/policy"
	"wanctl/internal/script"
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
// is used as the master secret for independently HKDF-deriving each namespace's
// controller identity and the rebind AEAD key (so reconnects keep a stable
// fingerprint without persisting private keys or exposing relay tokens).
//
// `endpointPath` is the public URL path the AI host POSTs to (usually "/mcp").
// mcp-go uses it to rewrite session URLs in responses; pass the same value you
// register the handler at.
func Handler(seed []byte, endpointPath string) (http.Handler, error) {
	if len(seed) < 32 {
		return nil, fmt.Errorf("mcp seed must be at least 32 bytes, got %d", len(seed))
	}
	sessions = &sessionStore{
		seed:    append([]byte(nil), seed...),
		m:       map[string]*remoteSession{},
		revoked: map[string]time.Time{},
	}
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
	return limits.HTTPServer(addr, mux).ListenAndServe()
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
	seed    []byte
	m       map[string]*remoteSession
	revoked map[string]time.Time // process-local JTI revocations; not durable across restart
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
		r = &remoteSession{
			id: sid, seed: s.seed, owner: s, known: transport.NewMemStore(),
			rebindJTIs: map[string]time.Time{}, lastUsed: time.Now(),
		}
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
	id         string
	seed       []byte
	owner      *sessionStore
	mu         sync.Mutex
	token      string
	namespace  string
	identity   *transport.Identity
	known      *transport.Store
	rebindJTIs map[string]time.Time
	lastUsed   time.Time
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

// saveLoginAndIssue atomically binds a remote session and records the newly
// issued rebind JTI so a subsequent logout can revoke it in this process.
func (r *remoteSession) saveLoginAndIssue(token, namespace string, now time.Time) (string, error) {
	if r.owner == nil {
		return "", fmt.Errorf("remote session has no owner")
	}
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.token, r.namespace, r.identity = token, namespace, nil
	if err := r.ensureIdentity(); err != nil {
		return "", err
	}
	credential, claim, err := sealRebind(r.owner.seed, namespace, token, now)
	if err != nil {
		return "", err
	}
	if r.rebindJTIs == nil {
		r.rebindJTIs = map[string]time.Time{}
	}
	r.rebindJTIs[claim.JTI] = time.Unix(claim.ExpiresAt, 0)
	return credential, nil
}

func (r *remoteSession) restoreLogin(claim rebindClaim, namespace string, now time.Time) error {
	if r.owner == nil {
		return fmt.Errorf("remote session has no owner")
	}
	if namespace != claim.Namespace {
		return fmt.Errorf("rebind namespace %q does not match Relay namespace %q", claim.Namespace, namespace)
	}
	if claim.ExpiresAt <= now.Unix() {
		return ErrExpiredRebind
	}
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	if expiry, revoked := r.owner.revoked[claim.JTI]; revoked {
		if now.Before(expiry) {
			return ErrRevokedRebind
		}
		delete(r.owner.revoked, claim.JTI)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.token, r.namespace, r.identity = claim.Token, namespace, nil
	if err := r.ensureIdentity(); err != nil {
		return err
	}
	if r.rebindJTIs == nil {
		r.rebindJTIs = map[string]time.Time{}
	}
	r.rebindJTIs[claim.JTI] = time.Unix(claim.ExpiresAt, 0)
	return nil
}

func (r *remoteSession) clearLogin() error {
	if r.owner != nil {
		r.owner.mu.Lock()
		defer r.owner.mu.Unlock()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.owner != nil {
		if r.owner.revoked == nil {
			r.owner.revoked = map[string]time.Time{}
		}
		now := time.Now()
		for jti, expiry := range r.owner.revoked {
			if !now.Before(expiry) {
				delete(r.owner.revoked, jti)
			}
		}
		for jti, expiry := range r.rebindJTIs {
			if now.Before(expiry) {
				r.owner.revoked[jti] = expiry
			}
		}
	}
	r.token, r.namespace, r.identity = "", "", nil
	r.known = transport.NewMemStore()
	r.rebindJTIs = map[string]time.Time{}
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
		mcpapi.WithDescription("Authenticate THIS MCP session to a wanctl namespace via the team portal. Two-step OAuth flow: (1) call with NO argument first → returns a portal URL + a one-time code prompt the user needs to complete in their browser. (2) call again with the `code` the user pastes back → exchanges it for a namespace token bound ONLY to this MCP session (in HTTP mode) or this machine's wanctl config (in stdio mode). Multiple AI users sharing the same MCP server each log in independently — credentials are never shared across sessions.\n\nFAST RE-BIND: a successful login also returns a `rebind` credential. HTTP-MCP sessions are in-memory, so a relay restart or a dropped/re-initialized connection can surface 'LOGIN REQUIRED' mid-task even though the user is still authorized. When that happens, call wanctl_login(rebind=\"…\") with the credential you saved — it restores access INSTANTLY with no Feishu round-trip. Only fall back to the OAuth flow if you have no saved rebind credential."),
		mcpapi.WithString("code", mcpapi.Description("The one-time code the user copied from the portal /enroll page. Omit on the first call.")),
		mcpapi.WithString("rebind", mcpapi.Description("A rebind credential returned by an earlier successful login in this conversation. Pass it to restore a lost session instantly without re-doing OAuth. Mutually exclusive with code.")),
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

	s.AddTool(mcpapi.NewTool("wanctl_pair",
		mcpapi.WithDescription("Check whether the target device already trusts this MCP session's controller identity, and if not, return the device-side pairing URL up front. On first contact this may instead return DEVICE IDENTITY CONFIRMATION REQUIRED; a human must independently verify its exact target and fingerprint, then call wanctl_trust_server before retrying. Once the server identity is pinned, returns '✓ already trusted' OR 'PAIRING REQUIRED' with a URL to relay VERBATIM to the user."),
		mcpapi.WithString("target", mcpapi.Required(), mcpapi.Description("Device name (DEVICE) or NS/DEVICE for shared devices.")),
	), mcpPair)

	s.AddTool(mcpapi.NewTool("wanctl_exec",
		mcpapi.WithDescription("Run a shell command, or a whole script, on a remote wanctl-enrolled device over the encrypted relay. Returns the device's stdout, stderr, and exit code. Pass EITHER 'command' (a one-liner) OR 'script' (multi-line source) — prefer 'script' for anything with a $, a quote inside a quote, or more than one statement, because a script is transported encoded and is never parsed by the device's shell. If the device hasn't paired this controller yet, the result is isError=true with a 'PAIRING REQUIRED' message that carries a URL — surface that URL VERBATIM to the user; do not paraphrase."),
		mcpapi.WithString("target", mcpapi.Required(), mcpapi.Description("Device name (DEVICE) or NS/DEVICE for shared devices. If exactly one device is online for this token, you may pass empty string.")),
		mcpapi.WithString("command", mcpapi.Description("A one-liner for the device's default shell (sh on Unix, powershell on Windows). WARNING: this string is SOURCE CODE for that shell and is parsed there. On Windows that means writing `powershell -Command \"...$x...\"` gets parsed TWICE — the outer shell expands $x to nothing and the inner script fails with a misleading 'term is not recognized'. Use 'script' instead of nesting an interpreter here.")),
		mcpapi.WithString("script", mcpapi.Description("Script SOURCE to run on the device (not a file path). Sent encoded, so quoting and character-set rules do not apply: $, backticks, nested quotes and non-ASCII text all arrive literally. Requires 'interp'. Use this for multi-statement work; it is the same single call as 'command'. Scripts over ~9KB must be pushed as a file and run by path instead.")),
		mcpapi.WithString("interp", mcpapi.Description("Interpreter for 'script': 'powershell' for Windows devices, 'sh' for Unix/macOS/Android. Required when 'script' is set.")),
		mcpapi.WithString("cwd", mcpapi.Description("Working directory on the device for this command (also the policy scope).")),
		mcpapi.WithBoolean("oneshot", mcpapi.Description("Run in a fresh shell with no persistent session state. Default false — successive exec calls share cwd/env like a real terminal.")),
	), mcpExec)

	s.AddTool(mcpapi.NewTool("wanctl_exec_async",
		mcpapi.WithDescription("Start a shell command as a BACKGROUND job on the device and return a job_id IMMEDIATELY, without waiting for it to finish. Use this for anything that may run longer than a single tool call comfortably tolerates — package installs, builds, large downloads, `wsl --shutdown` then a long build, etc. The command keeps running on the device even after this call returns; fetch its output and exit code later with wanctl_exec_poll(job_id). Always runs in a FRESH shell (no shared cwd/env with wanctl_exec's persistent session). Same pairing/policy rules as wanctl_exec. Jobs run for at most 30 minutes, retain at most 8 MiB output each, and finished results remain pollable for up to 1h subject to device-wide retention budgets."),
		mcpapi.WithString("target", mcpapi.Required(), mcpapi.Description("Device name (DEVICE) or NS/DEVICE.")),
		mcpapi.WithString("command", mcpapi.Required(), mcpapi.Description("Shell command to run in the device's default shell (sh on Unix, powershell on Windows).")),
		mcpapi.WithString("cwd", mcpapi.Description("Working directory on the device for this command (also the policy scope).")),
	), mcpExecAsync)

	s.AddTool(mcpapi.NewTool("wanctl_exec_poll",
		mcpapi.WithDescription("Fetch a background job's new output and status (started via wanctl_exec_async). Call repeatedly until state is 'done'. Pass the 'next_offset' from the previous poll as 'offset' to receive only NEW output each time; omit or 0 to get everything from the start. The response carries a status header (state: running|done, exit code when done, next_offset) followed by the output."),
		mcpapi.WithString("target", mcpapi.Required(), mcpapi.Description("Device name (DEVICE) or NS/DEVICE — the same device the job was started on.")),
		mcpapi.WithString("job_id", mcpapi.Required(), mcpapi.Description("The job id returned by wanctl_exec_async.")),
		mcpapi.WithNumber("offset", mcpapi.Description("Bytes of output already seen; return only output past this point. Use the previous poll's next_offset. Default 0 = from the start.")),
	), mcpExecPoll)

	s.AddTool(mcpapi.NewTool("wanctl_push",
		mcpapi.WithDescription("Upload a local file to a remote path on the target device. Same pairing/policy rules as wanctl_exec. NOTE: in HTTP (remote) MCP mode, 'local' is a path on the MCP SERVER, not on the AI host — this tool is intended for stdio mode where the AI host has direct filesystem access."),
		mcpapi.WithString("target", mcpapi.Required(), mcpapi.Description("Device name (DEVICE) or NS/DEVICE.")),
		mcpapi.WithString("local", mcpapi.Required(), mcpapi.Description("Absolute path on the MCP-server machine (or your local machine in stdio mode) to upload.")),
		mcpapi.WithString("remote", mcpapi.Required(), mcpapi.Description("Absolute path on the target device to write to.")),
	), mcpPush)

	s.AddTool(mcpapi.NewTool("wanctl_push_blob",
		mcpapi.WithDescription("Upload INLINE base64 content to a remote path on the target device — the file-push tool that works in HTTP (remote) MCP mode, where the AI host has no file on the MCP server for wanctl_push to read. Encode the bytes you want written as base64 and pass them in 'content_b64'. Same pairing/policy rules as wanctl_exec. Size cap: 8 MiB of raw (decoded) bytes; for larger payloads, split or have the device fetch the file itself."),
		mcpapi.WithString("target", mcpapi.Required(), mcpapi.Description("Device name (DEVICE) or NS/DEVICE.")),
		mcpapi.WithString("remote", mcpapi.Required(), mcpapi.Description("Absolute path on the target device to write to (overwrites if it exists).")),
		mcpapi.WithString("content_b64", mcpapi.Required(), mcpapi.Description("Standard-base64-encoded file content (the RAW bytes to write, not text).")),
		mcpapi.WithString("mode", mcpapi.Description("Optional octal file mode, e.g. \"0755\" for an executable. Default 0644.")),
	), mcpPushBlob)

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
		mcpapi.WithDescription("List the trust store for THIS MCP session. 'servers' (default) = explicitly pinned devices. 'clients' = controllers this machine has trusted to drive it (only meaningful in stdio mode if this machine is also running wanctl agent)."),
		mcpapi.WithString("which", mcpapi.Description("'servers' (default) or 'clients'.")),
	), mcpTrust)

	s.AddTool(mcpapi.NewTool("wanctl_trust_server",
		mcpapi.WithDescription("Confirm an unknown device identity for THIS MCP session. Call only after a human independently verifies the exact target and fingerprint returned by DEVICE IDENTITY CONFIRMATION REQUIRED. Certificate changes remain blocked unless replace=true is explicitly requested after re-verification."),
		mcpapi.WithString("target", mcpapi.Required(), mcpapi.Description("Exact owner/device target from the confirmation error.")),
		mcpapi.WithString("fingerprint", mcpapi.Required(), mcpapi.Description("Exact SHA256 fingerprint verified with the device owner.")),
		mcpapi.WithBoolean("replace", mcpapi.Description("Replace an existing pin after independent re-verification. Default false.")),
	), mcpTrustServer)

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
		"LOGIN REQUIRED. This MCP session has no wanctl credentials right now.\n" +
			"FIRST: if earlier in THIS conversation a wanctl_login succeeded and returned a `rebind` credential, the user is almost certainly still authorized — the in-memory session was just lost (relay restart / reconnect). Call wanctl_login(rebind=\"…\") with that saved credential to restore access INSTANTLY; do NOT bother the user. " +
			"ONLY if you have no saved rebind credential: call wanctl_login() (no args) — it returns a URL + instructions to show the user, who signs in via Feishu and pastes back a one-time code for wanctl_login(code=\"…\"). Then retry your previous tool call.",
	)
}

// --- auth tools ---

func mcpLogin(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	s := sessions.get(ctx)
	code := reqStr(req, "code", "")
	rebind := reqStr(req, "rebind", "")
	portal := config.EnvOr("WANCTL_PORTAL", config.DefaultPortal)
	if rebind != "" {
		r, ok := s.(*remoteSession)
		if !ok {
			return mcpapi.NewToolResultError("rebind credentials are only supported by the hosted HTTP MCP; run wanctl_login() again."), nil
		}
		claim, err := openRebind(r.owner.seed, rebind, time.Now())
		if err != nil {
			return mcpapi.NewToolResultError("rebind credential is invalid, expired, revoked, or from the legacy format; run wanctl_login() again to re-authenticate via the portal."), nil
		}
		tr := config.EnvOr("WANCTL_TRANSPORT", config.DefaultTransport)
		realNS, err := client.ResolveTokenNamespace(ctx, s.relayURL(), claim.Token, tr)
		if err != nil {
			return mcpapi.NewToolResultError("rebind token was rejected by the Relay; run wanctl_login() again to re-authenticate."), nil
		}
		if realNS != claim.Namespace {
			return mcpapi.NewToolResultError("rebind namespace does not match the Relay token owner; run wanctl_login() again to re-authenticate."), nil
		}
		if err := r.restoreLogin(claim, realNS, time.Now()); err != nil {
			if errors.Is(err, ErrRevokedRebind) {
				return mcpapi.NewToolResultError("rebind credential was revoked by logout; run wanctl_login() again to re-authenticate."), nil
			}
			return mcpapi.NewToolResultError(fmt.Sprintf("保存登录态失败: %s", err)), nil
		}
		return mcpapi.NewToolResultText(fmt.Sprintf("✓ 已用 rebind 凭证恢复到 namespace %q，并经 Relay 复核。可以继续之前的工具调用了。", realNS)), nil
	}
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
	var credential string
	if r, ok := s.(*remoteSession); ok {
		credential, err = r.saveLoginAndIssue(token, ns, time.Now())
	} else {
		err = s.saveLogin(token, ns)
	}
	if err != nil {
		return mcpapi.NewToolResultError(fmt.Sprintf("保存登录态失败: %s", err)), nil
	}
	msg := fmt.Sprintf(
		"✓ 已绑定到 namespace \"%s\". 现在可以调 wanctl_peers / wanctl_pair / wanctl_exec / wanctl_push / wanctl_pull / wanctl_logs 了。\n第一次拨某台设备没配对过的话有两种走法: (a) 直接 wanctl_exec — 会返回 'PAIRING REQUIRED' + 链接; (b) 想更友好,先 wanctl_pair(target=..) 拿到 URL 给用户. 不论走哪边,把链接原封不动给用户点开就行.",
		ns,
	)
	// HTTP-MCP credentials are in-memory and keyed by Mcp-Session-Id, so a relay
	// restart or a re-initialized connection can throw 'LOGIN REQUIRED' mid-task.
	// Hand the AI a rebind credential so it can self-heal without re-OAuth (#1).
	if credential != "" {
		msg += fmt.Sprintf(
			"\n\n⚠️ 给 AI 自己记(不必展示给用户): 保存下面这串 rebind 凭证。之后任何工具若返回 'LOGIN REQUIRED'(relay 重启/连接重置会触发,但用户其实仍有授权), 不要再打扰用户走飞书 —— 直接调 wanctl_login(rebind=\"…\") 即可瞬时恢复:\n\n  %s",
			credential)
	}
	return mcpapi.NewToolResultText(msg), nil
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

func mcpPair(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	target := reqStr(req, "target", "")
	if target == "" {
		return mcpapi.NewToolResultError("target is required"), nil
	}
	c, hint := sessions.get(ctx).client()
	if hint != nil {
		return hint, nil
	}
	trusted, pairingURL, err := c.Pair(ctx, target)
	if err != nil {
		// Could be: target offline, relay 404, token bad. Surface as plain
		// error — only the "pairing required" branch is special-cased.
		return mcpapi.NewToolResultError(err.Error()), nil
	}
	if trusted {
		return mcpapi.NewToolResultText(fmt.Sprintf("✓ %q 已经信任本 MCP session 的控制端身份, 无需手动配对. 可以直接调用 wanctl_exec / wanctl_push / wanctl_pull / wanctl_logs.", target)), nil
	}
	return mcpapi.NewToolResultText(fmt.Sprintf(
		"PAIRING REQUIRED. The target device has not yet trusted this MCP session's controller identity.\n\nGive this URL to the user VERBATIM (do not shorten, paraphrase, or wrap):\n\n%s\n\nAsk them to open it, click 「信任并继续」, then either call wanctl_pair again to confirm or just retry your data tool (wanctl_exec / push / pull / logs). URL is valid for 5 minutes.",
		pairingURL,
	)), nil
}

// execSource resolves the two ways to say "run this": a shell one-liner, or a
// script whose source must survive untouched.
//
// The distinction matters because 'command' is source for the device's shell —
// on Windows that is PowerShell, and an AI reaching for the familiar
// `powershell -Command "...$_..."` gets it parsed twice, losing every variable
// before the inner interpreter runs. 'script' sidesteps the shell entirely, so
// it is the right answer for anything beyond a one-liner.
func execSource(req mcpapi.CallToolRequest) (string, *mcpapi.CallToolResult) {
	command := reqStr(req, "command", "")
	src := reqStr(req, "script", "")
	switch {
	case command == "" && src == "":
		return "", mcpapi.NewToolResultError("pass either 'command' (a one-liner) or 'script' (source to run)")
	case command != "" && src != "":
		return "", mcpapi.NewToolResultError("pass either 'command' or 'script', not both")
	case src == "":
		return command, nil
	}
	interpName := reqStr(req, "interp", "")
	if interpName == "" {
		return "", mcpapi.NewToolResultError("'script' requires 'interp': 'powershell' for Windows devices, 'sh' for Unix/macOS/Android")
	}
	in, err := script.ParseInterp(interpName)
	if err != nil {
		return "", mcpapi.NewToolResultError(err.Error())
	}
	built, err := script.Command(in, []byte(src))
	if err != nil {
		return "", mcpapi.NewToolResultError(err.Error())
	}
	return built, nil
}

func mcpExec(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	target := reqStr(req, "target", "")
	command, errRes := execSource(req)
	if errRes != nil {
		return errRes, nil
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
		s := clampStream(stdout.Bytes())
		out += "\n--- stdout ---\n" + s
		if !strings.HasSuffix(s, "\n") {
			out += "\n"
		}
	}
	if stderr.Len() > 0 {
		s := clampStream(stderr.Bytes())
		out += "\n--- stderr ---\n" + s
		if !strings.HasSuffix(s, "\n") {
			out += "\n"
		}
	}
	if stdout.Len() == 0 && stderr.Len() == 0 {
		out += "(no output)\n"
	}
	return mcpapi.NewToolResultText(out), nil
}

func mcpExecAsync(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	target := reqStr(req, "target", "")
	command := reqStr(req, "command", "")
	if command == "" {
		return mcpapi.NewToolResultError("command is required"), nil
	}
	c, hint := sessions.get(ctx).client()
	if hint != nil {
		return hint, nil
	}
	id, err := c.ExecAsync(ctx, target, command, reqStr(req, "cwd", ""))
	if err != nil {
		if rej := asPairing(err); rej != nil {
			return pairingResult(rej), nil
		}
		return mcpapi.NewToolResultError(err.Error()), nil
	}
	return mcpapi.NewToolResultText(fmt.Sprintf(
		"started background job %s on %q.\nPoll it with wanctl_exec_poll(target=%q, job_id=%q) until state is 'done'. The job runs for at most 30 minutes and retains at most 8 MiB output; finished results remain available for up to 1h subject to device-wide retention budgets.",
		id, target, target, id)), nil
}

func mcpExecPoll(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	target := reqStr(req, "target", "")
	jobID := reqStr(req, "job_id", "")
	if jobID == "" {
		return mcpapi.NewToolResultError("job_id is required"), nil
	}
	c, hint := sessions.get(ctx).client()
	if hint != nil {
		return hint, nil
	}
	var buf bytes.Buffer
	newOffset, running, code, err := c.ExecPollTo(ctx, target, jobID, int64(reqInt(req, "offset")), &buf)
	if err != nil {
		if rej := asPairing(err); rej != nil {
			return pairingResult(rej), nil
		}
		return mcpapi.NewToolResultError(err.Error()), nil
	}
	head := fmt.Sprintf("state: running\nnext_offset: %d\n", newOffset)
	if !running {
		head = fmt.Sprintf("state: done\nexit: %d\nnext_offset: %d\n", code, newOffset)
	}
	out := head
	if buf.Len() > 0 {
		out += "\n--- new output ---\n" + clampStream(buf.Bytes())
	} else {
		out += "\n(no new output since offset)\n"
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

// maxBlobBytes caps the decoded size of a wanctl_push_blob upload. Inline base64
// rides the same request path as every other tool call, so keep it modest.
const maxBlobBytes = 8 << 20 // 8 MiB

func mcpPushBlob(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	target := reqStr(req, "target", "")
	remote := reqStr(req, "remote", "")
	b64 := reqStr(req, "content_b64", "")
	if remote == "" || b64 == "" {
		return mcpapi.NewToolResultError("remote and content_b64 are required"), nil
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return mcpapi.NewToolResultError("content_b64 is not valid standard base64: " + err.Error()), nil
	}
	if len(data) > maxBlobBytes {
		return mcpapi.NewToolResultError(fmt.Sprintf(
			"decoded content is %d bytes, over the %d-byte (8 MiB) inline cap; split it or have the device fetch the file itself",
			len(data), maxBlobBytes)), nil
	}
	var mode uint32
	if ms := strings.TrimSpace(reqStr(req, "mode", "")); ms != "" {
		m, perr := strconv.ParseUint(strings.TrimPrefix(ms, "0o"), 8, 32)
		if perr != nil {
			return mcpapi.NewToolResultError("mode must be octal like \"0644\": " + perr.Error()), nil
		}
		mode = uint32(m)
	}
	c, hint := sessions.get(ctx).client()
	if hint != nil {
		return hint, nil
	}
	if err := c.PushBytes(ctx, target, remote, data, mode); err != nil {
		if rej := asPairing(err); rej != nil {
			return pairingResult(rej), nil
		}
		return mcpapi.NewToolResultError(err.Error()), nil
	}
	return mcpapi.NewToolResultText(fmt.Sprintf("wrote %d bytes -> %s:%s", len(data), target, remote)), nil
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

func mcpTrustServer(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	target := reqStr(req, "target", "")
	fingerprint := reqStr(req, "fingerprint", "")
	if target == "" || fingerprint == "" {
		return mcpapi.NewToolResultError("target and fingerprint are required"), nil
	}
	c, hint := sessions.get(ctx).client()
	if hint != nil {
		return hint, nil
	}
	canonical, err := c.PinServer(ctx, target, fingerprint, reqBool(req, "replace"))
	if err != nil {
		return mcpapi.NewToolResultError(err.Error()), nil
	}
	return mcpapi.NewToolResultText(fmt.Sprintf("confirmed device identity: %s %s", canonical, fingerprint)), nil
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

// maxExecStream bounds how many bytes of one output stream (stdout/stderr) we
// return through the MCP layer. thunderbox's edge can silently truncate very
// large responses (see the /dl truncation pitfall), which is issue #13: the
// caller gets a cut-off result with no marker. We cap well below that and make
// any truncation EXPLICIT, keeping the head plus a larger tail (the tail usually
// holds the result/error the caller is after).
const maxExecStream = 48 * 1024

func clampStream(b []byte) string {
	if len(b) <= maxExecStream {
		return string(b)
	}
	const headN = 8 * 1024
	tailN := maxExecStream - headN
	dropped := len(b) - headN - tailN
	var sb strings.Builder
	sb.Write(b[:headN])
	sb.WriteString(fmt.Sprintf(
		"\n\n[... wanctl truncated %d bytes (%d total); showing first %d + last %d. "+
			"Re-run with a tighter filter, e.g. `... | Select-Object -Last 200` or `... | tail -n 200` ...]\n\n",
		dropped, len(b), headN, tailN))
	sb.Write(b[len(b)-tailN:])
	return sb.String()
}

// Compile-time anchor for indirectly-used packages.
var _ = http.StatusOK
