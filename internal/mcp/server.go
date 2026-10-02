// Package mcp wires the wanctl-flavored Model Context Protocol server. It
// supports two shapes:
//   - ServeStdio: a child process spawned by an AI host on the user's machine,
//     backed by the local wanctl config dir.
//   - Handler: the relay's hosted Streamable HTTP endpoint (multi-user). Every
//     request carries an OAuth 2.1 bearer, which names the namespace; the
//     controller identity is HKDF-derived per namespace, so reconnects keep a
//     stable fingerprint with no private-key persistence.
package mcp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"wanctl/internal/catalog"
	"wanctl/internal/client"
	"wanctl/internal/config"
	"wanctl/internal/mcpauth"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
	"wanctl/internal/script"
	"wanctl/internal/serverlog"
	"wanctl/internal/transport"

	mcpapi "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"golang.org/x/crypto/hkdf"
)

// ServeStdio runs the MCP server over stdio: single user, backed by the local
// wanctl config dir. Intended for AI hosts that spawn `wanctl mcp` as a child.
func ServeStdio() error {
	return ServeStdioWorkspaceSession(false)
}

// ServeStdioWorkspaceSession enables binding only when the host explicitly
// dedicates this process to one conversation; it is never an HTTP default.
func ServeStdioWorkspaceSession(enabled bool) error {
	sessions = &sessionStore{stdio: &localFsSession{}}
	if enabled {
		binding := newWorkspaceConversation()
		if raw := os.Getenv("WANCTL_WORKSPACE"); raw != "" {
			ref, err := client.ParseWorkspace(raw)
			if err != nil {
				return fmt.Errorf("WANCTL_WORKSPACE: %w", err)
			}
			binding.ref = ref.String()
		}
		defer binding.link.Close()
		return server.ServeStdio(newMCPServer(false, binding))
	}
	return server.ServeStdio(newMCPServer(false))
}

// Options configures the hosted MCP handler.
type Options struct {
	// Seed is the relay's MCP seed (≥32 bytes). It opens the OAuth access
	// tokens (internal/mcpauth) and is the master secret each namespace's
	// controller identity is HKDF-derived from.
	Seed []byte
	// EndpointPath is the public path the AI host POSTs to, usually "/mcp".
	// mcp-go uses it to rewrite session URLs in responses; pass the same value
	// the handler is registered at.
	EndpointPath string
	// OAuth is required: a bearer is the only way into the hosted endpoint.
	OAuth *OAuthConfig
}

// OAuthConfig is how the hosted endpoint authenticates: every request carries
// an OAuth 2.1 bearer, so identity never depends on the MCP session. Since
// v0.19.0 it is the only way in. The code-and-rebind login that used to work
// without it kept its logouts in process memory, so a relay restart revived a
// logged-out credential (CX-04); see docs/adr/0008-mcp-oauth-split-portal-relay.md.
type OAuthConfig struct {
	// ResourceMetadataURL is what a 401 points the client at so it can start
	// the authorization flow (RFC 9728).
	ResourceMetadataURL string
	// Live reports whether the namespace token inside an access token is still
	// valid. Asked on every bearer request, so revoking a grant takes effect on
	// the next call rather than when the hour-long token expires.
	Live func(namespace, token string) bool
	// Revoke stops the namespace token behind a grant. It is what wanctl_logout
	// does on this path: the bearer is stateless, so the only durable way to
	// end the session is to end the token it carries.
	Revoke func(namespace, token string) error
}

// Handler returns the hosted Streamable HTTP MCP endpoint, for the relay to
// mount at /mcp. A request without a valid OAuth bearer is answered 401 with
// the challenge that starts a client's authorization flow, before mcp-go or
// any tool sees it.
func Handler(o Options) (http.Handler, error) {
	if len(o.Seed) < 32 {
		return nil, fmt.Errorf("mcp seed must be at least 32 bytes, got %d", len(o.Seed))
	}
	if o.OAuth == nil || o.OAuth.ResourceMetadataURL == "" {
		return nil, errors.New("hosted MCP authenticates with OAuth only; it needs the relay's OAuth server (DATABASE_URL, WANCTL_PUBLIC_ORIGIN, WANCTL_PORTAL)")
	}
	store := &sessionStore{
		seed:  append([]byte(nil), o.Seed...),
		m:     map[string]*remoteSession{},
		open:  map[string]*transportSession{},
		trust: map[string]*transport.Store{},
		oauth: o.OAuth,
	}
	sessions = store
	go store.gcLoop()
	s := newMCPServer(true)
	// Session IDs come from the store, which bounds and expires them; see
	// hostedsessions.go.
	opts := []server.StreamableHTTPOption{server.WithSessionIdManager(sessionIDs{store: store})}
	if o.EndpointPath != "" {
		opts = append(opts, server.WithEndpointPath(o.EndpointPath))
	}
	// The bearer is verified by the gate in front, which can answer 401 with a
	// WWW-Authenticate header; mcp-go answers in JSON-RPC and has no way to.
	// This carries the gate's verdict the rest of the way in.
	opts = append(opts, server.WithHTTPContextFunc(func(ctx context.Context, req *http.Request) context.Context {
		if claim, ok := oauthClaimFrom(req.Context()); ok {
			return context.WithValue(ctx, oauthClaimKey{}, claim)
		}
		return ctx
	}))
	streamable := server.NewStreamableHTTPServer(s, opts...)
	store.terminate = terminateVia(streamable)
	return oauthGate(append([]byte(nil), o.Seed...), o.OAuth, refuseBrowserOrigins(store.gate(streamable))), nil
}

// Unavailable is what a relay mounts at /mcp when it cannot run the hosted
// endpoint, so a client that was pointed there is told why instead of getting
// a bare 404.
func Unavailable(reason string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": nil,
			"error": map[string]any{"code": -32000, "message": reason},
		})
	})
}

type oauthClaimKey struct{}

func oauthClaimFrom(ctx context.Context) (mcpauth.Claim, bool) {
	claim, ok := ctx.Value(oauthClaimKey{}).(mcpauth.Claim)
	return claim, ok
}

// oauthGate authenticates the bearer before mcp-go sees the request. Every
// request needs one. A request with no Authorization at all gets the bare
// challenge (RFC 6750 §3.1: no error code when no credential was offered),
// which is what sends an MCP client to the resource metadata and on into the
// authorization flow.
func oauthGate(seed []byte, cfg *OAuthConfig, next http.Handler) http.Handler {
	challenge := `Bearer resource_metadata="` + cfg.ResourceMetadataURL + `"`
	deny := func(w http.ResponseWriter, code, description string) {
		// invalid_request is sent only when no credential was offered at all,
		// and then the challenge carries no error code (RFC 6750 §3.1).
		header := challenge
		if code != "invalid_request" {
			header += `, error="` + code + `", error_description="` + description + `"`
		}
		w.Header().Set("WWW-Authenticate", header)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"` + code + `","error_description":"` + description + `"}`))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw := strings.TrimSpace(req.Header.Get("Authorization"))
		if raw == "" {
			deny(w, "invalid_request", "this endpoint needs an OAuth 2.1 access token; authorize to get one")
			return
		}
		bearer := ""
		if len(raw) > 7 && strings.EqualFold(raw[:7], "Bearer ") {
			bearer = strings.TrimSpace(raw[7:])
		}
		// A client that sent some other credential is told where to get a real
		// one rather than being let through unauthenticated: it asked to be
		// authenticated, and silently ignoring that would leave it convinced it
		// was logged in as somebody.
		if bearer == "" || !mcpauth.IsAccessToken(bearer) {
			deny(w, "invalid_token", "this endpoint accepts OAuth 2.1 access tokens; authorize to get one")
			return
		}
		claim, err := mcpauth.OpenAccess(seed, bearer, time.Now())
		if err != nil {
			deny(w, "invalid_token", "access token is invalid or expired; refresh or authorize again")
			return
		}
		if cfg.Live != nil && !cfg.Live(claim.Namespace, claim.Token) {
			deny(w, "invalid_token", "this authorization was revoked; authorize again")
			return
		}
		next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), oauthClaimKey{}, claim)))
	})
}

// refuseBrowserOrigins turns away requests that carry an Origin header the
// operator did not allow. MCP clients are programs and send no Origin; a
// browser page always does, and a page the operator merely visited could
// otherwise DNS-rebind to a bound address and drive it (audit 2026-08-28,
// SEC-E-05). WANCTL_MCP_ALLOWED_ORIGINS
// is a comma-separated allow-list for deliberate browser-based hosts.
func refuseBrowserOrigins(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if origin := req.Header.Get("Origin"); origin != "" && !originAllowed(origin) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, req)
	})
}

func originAllowed(origin string) bool {
	for _, a := range strings.Split(os.Getenv("WANCTL_MCP_ALLOWED_ORIGINS"), ",") {
		if a = strings.TrimSpace(a); a != "" && strings.EqualFold(a, origin) {
			return true
		}
	}
	return false
}

// --- session abstraction ---

// sessionAPI is the per-client backing the MCP tools talk to. Two impls:
//   - localFsSession (stdio): persists to wanctl's local config dir
//   - remoteSession (http): an OAuth bearer's state + namespace-derived identity
type sessionAPI interface {
	client() (*client.Client, *mcpapi.CallToolResult)
	clearLogin() error
	info() string // for wanctl_status
	relayURL() (string, error)
	// localFile decides whether a tool may open path on the machine the MCP
	// server runs on, and returns the cleaned absolute path to use. The
	// answer depends on who that machine belongs to: in stdio mode it is the
	// operator's own, in HTTP mode it is shared infrastructure (SEC-E-01).
	localFile(path string, write bool) (string, *mcpapi.CallToolResult)
}

type sessionStore struct {
	mu sync.Mutex

	// stdio mode
	stdio *localFsSession

	// http mode
	seed []byte
	// m is the state behind each bearer, keyed by the access token's JTI
	// rather than by Mcp-Session-Id: the same bearer reaching us in two
	// unrelated MCP sessions is one logged-in person, not two strangers.
	m map[string]*remoteSession
	// open is every transport session admitted and not yet closed, keyed by
	// Mcp-Session-Id (hostedsessions.go). terminate has mcp-go free its own
	// state for one; clock stands in for time.Now in tests.
	open      map[string]*transportSession
	terminate func(id string)
	clock     func() time.Time

	oauth *OAuthConfig
	// trust is the pinned-server store, shared by namespace. A per-session
	// store cannot work here — a client that opens a new session per call would
	// arrive un-pinned every time and be asked to confirm the same device
	// identity forever. Process-local, so a relay restart asks once more.
	trust map[string]*transport.Store
}

var sessions *sessionStore

var serverLogsHTTPClient = http.DefaultClient

func (s *sessionStore) get(ctx context.Context) sessionAPI {
	if s.stdio != nil {
		return s.stdio
	}
	if claim, ok := oauthClaimFrom(ctx); ok {
		return s.oauthSession(claim)
	}
	// The gate answers every hosted request without a valid bearer with 401,
	// so a tool call cannot get here. Should that ever stop being true, a
	// session with no token reaches no device.
	return &remoteSession{seed: s.seed, owner: s}
}

// oauthSession returns the session a bearer names, already logged in. There is
// no login step on this path: the browser trip that minted the token was the
// login, and the token carries the namespace and the relay credential.
func (s *sessionStore) oauthSession(claim mcpauth.Claim) sessionAPI {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := "oauth:" + claim.JTI
	r := s.m[key]
	if r == nil {
		r = &remoteSession{
			id: key, seed: s.seed, owner: s,
			known:      s.trustForLocked(claim.Namespace),
			oauthClaim: claim,
			token:      claim.Token, namespace: claim.Namespace,
		}
		s.m[key] = r
	}
	r.lastUsed = s.now()
	return r
}

// trustForLocked hands every session in a namespace the same pinned-server
// store. Caller holds s.mu.
func (s *sessionStore) trustForLocked(namespace string) *transport.Store {
	if s.trust == nil {
		s.trust = map[string]*transport.Store{}
	}
	if store := s.trust[namespace]; store != nil {
		return store
	}
	store := transport.NewMemStore()
	s.trust[namespace] = store
	return store
}

// forgetPin drops whatever identity is pinned under one namespace/device, for
// every hosted session of that namespace: they all share one store.
//
// Without it the hosted pin is the dead end ADR 0002 describes: a reinstalled
// device presents a new certificate, every call fails with an identity
// mismatch, and nothing short of restarting the relay clears it — the store is
// process memory, so there is no file an operator could delete either. The
// portal answers this by forgetting its pin when the owner unbinds the device
// (Server.forgetPin), and unbinding is the owner saying over SSO that this
// name no longer refers to that machine. This is the same rule for the same
// reason; the relay calls it through ForgetPinnedDevice.
func (s *sessionStore) forgetPin(namespace, device string) {
	if s == nil || namespace == "" || device == "" {
		return
	}
	s.mu.Lock()
	store := s.trust[namespace]
	s.mu.Unlock()
	if store != nil {
		_ = store.RemoveName(namespace + "/" + device)
	}
}

// ForgetPinnedDevice is the hook the relay calls when an owner unbinds a
// device, so the hosted MCP store forgets it the way the portal's does.
// Deliberately not called on re-registration: a device that came back with a
// new certificate clearing its own alarm is the alternative ADR 0002 rejected.
// Safe before any handler exists, and a no-op over stdio.
func ForgetPinnedDevice(namespace, device string) {
	sessions.forgetPin(namespace, device)
}

// gcLoop closes idle HTTP sessions every minute; see sweep for the limits.
func (s *sessionStore) gcLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		s.sweep()
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

// localFile confines wanctl_push/wanctl_pull on the operator's machine. The
// model may be prompt-injected, so the default root is the directory in which
// the MCP server was started; WANCTL_MCP_LOCAL_ROOT may name a different tree
// explicitly. wanctl's credential/config directory is refused independently
// of either root.
func (l *localFsSession) localFile(path string, _ bool) (string, *mcpapi.CallToolResult) {
	abs, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return "", mcpapi.NewToolResultError("local: " + err.Error())
	}
	abs = filepath.Clean(abs)
	root := strings.TrimSpace(os.Getenv("WANCTL_MCP_LOCAL_ROOT"))
	rootLabel := "the MCP server's working directory"
	if root == "" {
		root, err = os.Getwd()
		if err != nil {
			return "", mcpapi.NewToolResultError("resolve MCP working directory: " + err.Error())
		}
	} else {
		rootLabel = "WANCTL_MCP_LOCAL_ROOT"
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", mcpapi.NewToolResultError(rootLabel + ": " + err.Error())
	}
	rootAbs = resolveExisting(rootAbs)
	cand := resolveExisting(abs)
	if !pathWithin(cand, rootAbs) {
		return "", mcpapi.NewToolResultError(fmt.Sprintf(
			"local path %s is outside %s (%s); set WANCTL_MCP_LOCAL_ROOT to the exact tree the MCP server may access", abs, rootLabel, rootAbs))
	}
	if dir := config.SettingsDir(); dir != "" {
		dir = resolveExisting(dir)
		if pathWithin(cand, dir) {
			return "", mcpapi.NewToolResultError("local path is inside wanctl's own config dir (identity, token, trust); refused")
		}
	}
	return abs, nil
}

func pathWithin(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// resolveExisting follows symlinks on the longest existing prefix of p and
// re-attaches the rest, so a path that does not exist yet still compares
// against a resolved root (macOS puts $TMPDIR under /var → /private/var).
func resolveExisting(p string) string {
	rest := ""
	for cur := p; ; {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

func (l *localFsSession) clearLogin() error { return config.ClearToken() }
func (l *localFsSession) relayURL() (string, error) {
	relay, err := config.Relay()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(relay, "/"), nil
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
	out += "relay:                 " + configuredValue(settingValue("relay")) + "\n"
	out += "portal:                " + configuredValue(settingValue("portal")) + "\n"
	out += "config dir:            " + dir + "  (override with WANCTL_CONFIG_DIR for per-AI-user isolation)\n"
	return out
}

// --- remote (HTTP) session: one per OAuth bearer, ephemeral, identity derived ---

type remoteSession struct {
	id         string
	seed       []byte
	owner      *sessionStore
	mu         sync.Mutex
	token      string
	namespace  string
	identity   *transport.Identity
	known      *transport.Store
	lastUsed   time.Time
	oauthClaim mcpauth.Claim
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

// localFile on a shared MCP server: there is no legitimate "local" file. The
// machine is the relay's own container or a multi-tenant host, so a path here
// is a path on shared infrastructure — /proc/self/environ would hand any
// logged-in tenant the relay's admin secret and database credentials
// (audit 2026-08-28, SEC-E-01). wanctl_push_blob and wanctl_exec cover what
// these tools were for.
func (r *remoteSession) localFile(string, bool) (string, *mcpapi.CallToolResult) {
	return "", mcpapi.NewToolResultError("wanctl_push/wanctl_pull are unavailable on a shared (HTTP) MCP server: " +
		"'local' would be a path on the server itself. Upload with wanctl_push_blob; read remote files with wanctl_exec (e.g. cat/base64).")
}

func (r *remoteSession) client() (*client.Client, *mcpapi.CallToolResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.token == "" {
		return nil, notAuthorized()
	}
	if err := r.ensureIdentity(); err != nil {
		return nil, mcpapi.NewToolResultError("derive identity: " + err.Error())
	}
	relay, err := config.Relay()
	if err != nil {
		return nil, mcpapi.NewToolResultError(err.Error())
	}
	relay = strings.TrimRight(relay, "/")
	tr := config.EnvOr("WANCTL_TRANSPORT", config.DefaultTransport)
	c := client.NewWith(r.identity, r.known, relay, r.token, tr)
	// Devices refuse a pairing request from a controller that will not say who it
	// is, and an HTTP MCP session has no config dir to read a label from. Name the
	// session for what it actually is, so the owner approving it knows an AI is
	// on the other end rather than seeing the relay's hostname.
	c.SetLabel(config.EnvOr("WANCTL_LABEL", "AI 助手 · MCP 会话 (ns: "+r.namespace+")"))
	return c, nil
}

// clearLogin is wanctl_logout. Forgetting the token in this process would mean
// nothing: the bearer carries it, and the next request rebuilds the session
// from it. Logging out has to reach the relay and kill the namespace token.
func (r *remoteSession) clearLogin() error {
	if r.owner == nil || r.owner.oauth == nil || r.owner.oauth.Revoke == nil {
		return fmt.Errorf("this endpoint cannot revoke OAuth grants; revoke the token in the portal instead")
	}
	if err := r.owner.oauth.Revoke(r.namespace, r.token); err != nil {
		return err
	}
	r.owner.mu.Lock()
	delete(r.owner.m, r.id)
	r.owner.mu.Unlock()
	r.mu.Lock()
	r.token, r.namespace, r.identity = "", "", nil
	r.mu.Unlock()
	return nil
}

func (r *remoteSession) relayURL() (string, error) {
	relay, err := config.Relay()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(relay, "/"), nil
}

func (r *remoteSession) info() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	// A client may open a new MCP session per call, so the session id is
	// noise. What answers "am I logged in" is the bearer.
	out := "mode:                  http (remote, OAuth bearer)\n"
	out += "client:                " + r.oauthClaim.ClientID + "\n"
	out += "access token expires:  " + r.oauthClaim.Expiry().UTC().Format(time.RFC3339) + "\n"
	if r.token == "" {
		out += "status:                NOT authorized — this connection's grant was revoked; authorize the connector again\n"
	} else {
		out += "status:                logged in to namespace \"" + r.namespace + "\"\n"
		if r.identity != nil {
			out += "controller fingerprint: " + r.identity.Fingerprint + "\n"
		}
	}
	out += "relay:                 " + configuredValue(hostedRelayDisplay()) + "\n"
	out += "portal:                " + configuredValue(settingValue("portal")) + "\n"
	out += "note:                  identity is derived per-namespace, so the same person reconnecting keeps the same fingerprint (no re-pairing).\n"
	return out
}

// hostedRelayDisplay is the relay address to show in HTTP mode. A hosted relay
// runs the MCP server in-process and dials itself over loopback, so the
// configured WANCTL_RELAY is an address no caller of this tool could ever
// reach. WANCTL_PUBLIC_ORIGIN is that same relay's public name. Display only —
// relayURL() still returns the address that gets dialed.
func hostedRelayDisplay() string {
	if origin := strings.TrimRight(os.Getenv("WANCTL_PUBLIC_ORIGIN"), "/"); origin != "" {
		return origin
	}
	return settingValue("relay")
}

// settingValue is config.Setting without the source, for display lines.
func settingValue(key string) string {
	v, _ := config.Setting(key)
	return v
}

func configuredValue(value string) string {
	if value == "" {
		return "(not configured)"
	}
	return value
}

// --- tool registration (shared between stdio and http modes) ---

// registerMCPTools registers every tool the catalog declares. The tool names,
// descriptions and schemas all come from internal/catalog, which is the same
// source `wanctl help` renders and docs/contract.md is generated from — so an
// AI reading a tool description and a human reading the help see one text, not
// two that drifted. What stays here is the handler behind each name.
// newMCPServer builds the server both transports serve: the same tools, and the
// same instructions, less the local login on the hosted endpoint, whose bearer
// already is the login.
//
// The instructions are the catalog's — what wanctl is, every primitive in one
// list, the loop they compose into, the refusals to recognise. A host reads
// them once, before any tool call, which is the only moment at which "read the
// project's AGENTS.md first" can still change what happens.
func newMCPServer(hosted bool, bindings ...*workspaceConversation) *server.MCPServer {
	instructions := catalog.Instructions()
	switch {
	case hosted:
		instructions = catalog.HostedInstructions()
	case len(bindings) > 0:
		instructions = catalog.WorkspaceSessionInstructions
	}
	s := server.NewMCPServer("wanctl", "1.0.0", server.WithInstructions(instructions), server.WithOutputSchemaValidation())
	registerMCPTools(s, hosted, bindings...)
	return s
}

func registerMCPTools(s *server.MCPServer, hosted bool, bindings ...*workspaceConversation) {
	handlers := map[string]server.ToolHandlerFunc{
		"mcpLogin":       mcpLogin,
		"mcpStatus":      mcpStatus,
		"mcpLogout":      mcpLogout,
		"mcpPeers":       mcpPeers,
		"mcpPair":        mcpPair,
		"mcpExec":        mcpExec,
		"mcpWorkspace":   mcpWorkspace,
		"mcpRead":        mcpRead,
		"mcpEdit":        mcpEdit,
		"mcpWrite":       mcpWrite,
		"mcpScreenshot":  mcpScreenshot,
		"mcpExecAsync":   mcpExecAsync,
		"mcpExecPoll":    mcpExecPoll,
		"mcpPush":        mcpPush,
		"mcpPushBlob":    mcpPushBlob,
		"mcpPull":        mcpPull,
		"mcpLogs":        mcpLogs,
		"mcpServerLogs":  mcpServerLogs,
		"mcpID":          mcpID,
		"mcpTrust":       mcpTrust,
		"mcpTrustServer": mcpTrustServer,
		"mcpRules":       mcpRules,
	}
	for _, c := range catalog.MCPCommands() {
		if len(bindings) > 0 && !workspaceSessionTool(c.MCPName) {
			continue
		}
		if hosted && c.StdioOnly {
			continue
		}
		h, ok := handlers[c.Handler]
		if !ok {
			// A catalog entry naming a handler that does not exist would
			// otherwise register a tool that answers nothing. Fail at startup,
			// where the first test run catches it.
			panic("mcp: catalog tool " + c.MCPName + " names unknown handler " + c.Handler)
		}
		if len(bindings) > 0 {
			h = bindings[0].wrap(c.MCPName, h)
			if workspaceDataTool(c.MCPName) {
				params := []catalog.Param{}
				for _, p := range c.Params {
					if p.Name != "target" && p.Name != "workspace" {
						params = append(params, p)
					}
				}
				c.Params = params
				c.Desc = "CONVERSATION MODE: the entered workspace is injected automatically. Do not provide target or workspace.\n\n" + c.Desc
			}
		}
		s.AddTool(mcpapi.NewTool(c.MCPName, toolOptions(c)...), escapeDeviceOutput(c.MCPName, h))
	}
}

// toolOptions turns one catalog entry into the mcp-go options that describe it.
func toolOptions(c catalog.Command) []mcpapi.ToolOption {
	opts := []mcpapi.ToolOption{mcpapi.WithDescription(c.MCPDescription())}
	if schema := c.MCPOutputSchema(); schema != nil {
		raw, err := json.Marshal(schema)
		if err != nil {
			panic("mcp: invalid output schema for " + c.MCPName + ": " + err.Error())
		}
		opts = append(opts, mcpapi.WithRawOutputSchema(raw))
	}
	for _, p := range c.MCPParams() {
		var props []mcpapi.PropertyOption
		if p.Required {
			props = append(props, mcpapi.Required())
		}
		props = append(props, mcpapi.Description(p.Desc))
		switch p.Type {
		case catalog.TypeString:
			opts = append(opts, mcpapi.WithString(p.Name, props...))
		case catalog.TypeBool:
			opts = append(opts, mcpapi.WithBoolean(p.Name, props...))
		case catalog.TypeNumber:
			opts = append(opts, mcpapi.WithNumber(p.Name, props...))
		case catalog.TypeArray:
			// The item schema is what tells a caller the shape of the objects
			// it may put in the list. Without it the argument is "an array of
			// something" and a model has to infer {old,new} from the prose.
			if p.Items != nil {
				props = append(props, mcpapi.Items(p.Items))
			}
			opts = append(opts, mcpapi.WithArray(p.Name, props...))
		default:
			panic("mcp: catalog parameter " + c.MCPName + "." + p.Name + " has unknown type " + p.Type)
		}
	}
	return opts
}

// --- helpers ---

// Preserve the existing human-readable content while adding machine-readable
// fields. Errors keep their existing isError/diagnostic contract; do not turn
// uncertain writes or failed authorizations into successful-looking objects.
func structuredResult(text string, data map[string]any) *mcpapi.CallToolResult {
	result := mcpapi.NewToolResultText(text)
	result.StructuredContent = data
	return result
}

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

// hostedSession reports whether the caller reached us over the shared HTTP
// endpoint. That, not an env var, is what decides who an error is written for:
// over stdio a human is at a terminal and can run the CLI, while over HTTP the
// only reader is a model whose host already prompts its user before every tool
// call.
func hostedSession(sess sessionAPI) bool {
	_, hosted := sess.(*remoteSession)
	return hosted
}

// dialErrorResult turns a failed dial into the result the caller should act on.
// Every tool that reaches a device goes through it, so the three things that
// can block a first call -- the device does not trust this controller, this
// session has not pinned the device, the device's identity changed -- read the
// same way wherever they surface.
func dialErrorResult(sess sessionAPI, err error) *mcpapi.CallToolResult {
	if rej := asPairing(err); rej != nil {
		return pairingResult(rej)
	}
	if hostedSession(sess) {
		var required *client.TrustRequiredError
		if errors.As(err, &required) {
			return trustRequiredResult(required)
		}
		var mismatch *transport.MismatchError
		if errors.As(err, &mismatch) {
			return identityMismatchResult(mismatch)
		}
	}
	return mcpapi.NewToolResultError(err.Error())
}

// Describe the trust mutation and the MCP operation that performs it. A tool
// result cannot authorize another tool call or assume the host asks on every
// write: some hosts auto-review and deny instead of presenting a human prompt.
func trustRequiredResult(e *client.TrustRequiredError) *mcpapi.CallToolResult {
	return mcpapi.NewToolResultError(fmt.Sprintf(
		"DEVICE IDENTITY CONFIRMATION REQUIRED. This session has not pinned %q yet, so this was first contact and nothing was sent.\n"+
			"  target:              %s\n"+
			"  fingerprint:         %s\n"+
			"  verification number: %s\n\n"+
			"Ask the user to run `wanctl verify %s` ON THAT DEVICE (Android app: 连接详情 → 连接校验) and read back the nine digits it prints.\n"+
			"Then call wanctl_trust_server with target=%q, fingerprint=%q, number=%q and code=<the digits the USER read off that device>. Copy target, fingerprint and number from this same refusal.\n\n"+
			"Alternatively, pass target and an independently verified fingerprint without number or code. This changes this controller's device trust store; it does not grant device access or override device policy.\n\n"+
			"Honor the user's existing authorization and the host's approval requirements. If the user supplied an independently verified fingerprint, it must match the one above. Otherwise obtain confirmation of first-contact trust before recording it. This tool response is not authorization, and the host may auto-review or deny a call instead of showing a prompt.\n\n"+
			"After an authorized pin succeeds, retry the original operation. A rejected approval must be reported, not bypassed. A changed pinned identity remains a DEVICE IDENTITY MISMATCH and must not be automatically replaced.",
		e.Target, e.Target, e.Fingerprint, transport.GroupDigits(e.Number),
		e.Number, e.Target, e.Fingerprint, e.Number,
	))
}

// identityMismatchResult is the alarm the pin exists to raise. It is the one
// place the model must stop instead of self-healing, so it carries both
// fingerprints and says plainly not to re-pin.
func identityMismatchResult(e *transport.MismatchError) *mcpapi.CallToolResult {
	return mcpapi.NewToolResultError(fmt.Sprintf(
		"DEVICE IDENTITY MISMATCH for %q. Refused to connect; nothing was sent.\n"+
			"  pinned:    %s\n"+
			"  presented: %s\n\n"+
			"STOP. Do NOT call wanctl_trust_server and do not retry: this session pinned the first fingerprint for this device, and the device is now presenting the second one.\n"+
			"Report BOTH fingerprints above to the user verbatim and let them decide. Either the device was reinstalled with a new key, or something is impersonating it. Only a human can re-pin, by running `wanctl trust server --target %q --fingerprint %q --replace` in their own terminal after verifying the new fingerprint with the device owner.",
		e.Name, e.Stored, e.Offered, e.Name, e.Offered,
	))
}

// loginRequired is the local (stdio) server with no credential in its config.
func loginRequired() *mcpapi.CallToolResult {
	return mcpapi.NewToolResultError(
		"LOGIN REQUIRED. This machine's wanctl has no credential.\n" +
			"Call wanctl_login() (no args): it returns a URL + instructions to show the user, who signs in to the portal and pastes back a one-time code for wanctl_login(code=\"…\"). Then retry your previous tool call.",
	)
}

// notAuthorized is a hosted session whose grant this connection just revoked
// with wanctl_logout. Every later request is answered 401 by the gate.
func notAuthorized() *mcpapi.CallToolResult {
	return mcpapi.NewToolResultError(
		"NOT AUTHORIZED. This connection's OAuth grant was revoked. The user has to authorize the connector again in their AI host; there is nothing to call here.")
}

// --- auth tools ---

// mcpLogin is the local (stdio) server's login: the portal's one-time code,
// exchanged for a token saved in this machine's wanctl config, exactly as
// `wanctl login` does. The hosted endpoint does not register it: its bearer is
// the login.
func mcpLogin(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	s := sessions.get(ctx)
	code := reqStr(req, "code", "")
	portal, err := config.Portal()
	if err != nil {
		return mcpapi.NewToolResultError(err.Error()), nil
	}
	if code == "" {
		return mcpapi.NewToolResultText(fmt.Sprintf(
			"OK — drive the user through the portal sign-in to mint a session token. Show them these instructions VERBATIM:\n\n"+
				"  1. Open: %s/enroll\n"+
				"  2. Sign in to the portal (they are likely signed in already).\n"+
				"  3. Copy the big one-time code shown on that page (e.g. ABCD-1234).\n"+
				"  4. Paste it back to me.\n\n"+
				"When they paste the code, call wanctl_login again with code=\"…\" to complete the login.",
			portal,
		)), nil
	}
	// An MCP session is a controller, not a device: it never runs an agent, so
	// the enrollment's portal fingerprint has nothing to seed here.
	relayURL, err := s.relayURL()
	if err != nil {
		return mcpapi.NewToolResultError(err.Error()), nil
	}
	en, err := client.ExchangeCode(ctx, relayURL, code)
	if err != nil {
		return mcpapi.NewToolResultError(fmt.Sprintf("授权失败: %s\n请让用户回到 %s/enroll 拿一个新 code（旧的可能用过或过期了），然后再调一次 wanctl_login(code=\"…\")。", err, portal)), nil
	}
	if err := config.SaveToken(en.Token); err != nil {
		return mcpapi.NewToolResultError(fmt.Sprintf("保存登录态失败: %s", err)), nil
	}
	return mcpapi.NewToolResultText(fmt.Sprintf(
		"✓ 已绑定到 namespace \"%s\". 现在可以调 wanctl_peers / wanctl_pair / wanctl_exec / wanctl_push / wanctl_pull / wanctl_logs 了。\n第一次拨某台设备没配对过的话有两种走法: (a) 直接 wanctl_exec — 会返回 'PAIRING REQUIRED' + 链接; (b) 想更友好,先 wanctl_pair(target=..) 拿到 URL 给用户. 不论走哪边,把链接原封不动给用户点开就行.",
		en.Namespace,
	)), nil
}

func mcpStatus(ctx context.Context, _ mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	return mcpapi.NewToolResultText(sessions.get(ctx).info()), nil
}

func mcpLogout(ctx context.Context, _ mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	s := sessions.get(ctx)
	if err := s.clearLogin(); err != nil {
		return mcpapi.NewToolResultError(err.Error()), nil
	}
	if hostedSession(s) {
		return mcpapi.NewToolResultText(
			"✓ 已吊销这份 OAuth 授权对应的中继令牌。这个连接器的后续调用会收到 401，再用需要用户在浏览器里重新授权一次。"), nil
	}
	return mcpapi.NewToolResultText("✓ 已清除本会话凭证. 想再操作请先 wanctl_login()."), nil
}

// --- data tools ---

func mcpPeers(ctx context.Context, _ mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	c, hint := sessions.get(ctx).client()
	if hint != nil {
		return hint, nil
	}
	view, err := c.PeersAndShared(ctx)
	if err != nil {
		return mcpapi.NewToolResultError(err.Error()), nil
	}
	return peerToolResult(view, pinStateOf(c, view)), nil
}

// pinStateOf answers "have I pinned this device's identity yet" for every
// device in the listing. It reads the session's known-servers store and
// nothing else: dialing each device to find out would turn a listing into a
// fan-out of connections, and on first contact would fail anyway.
//
// The store is keyed by the canonical "namespace/device" — the same name the
// pin is written under — so an own device has to be qualified with the
// namespace the relay reported, and a shared device already carries it.
func pinStateOf(c *client.Client, view client.Peers) map[string]bool {
	pinned := map[string]bool{}
	for _, d := range view.Devices {
		name := canonicalTarget(view.Namespace, d)
		_, ok := c.Pinned(name)
		pinned[name] = ok
	}
	for _, sd := range view.Shared {
		_, ok := c.Pinned(sd.Target)
		pinned[sd.Target] = ok
	}
	return pinned
}

func canonicalTarget(namespace, device string) string {
	if namespace == "" || strings.Contains(device, "/") {
		return device
	}
	return namespace + "/" + device
}

// identityNote is the suffix that tells a model, before it dials anything,
// which devices will stop it with DEVICE IDENTITY CONFIRMATION REQUIRED.
func identityNote(pinned map[string]bool, name string) string {
	if pinned[name] {
		return "  [identity: pinned]"
	}
	return "  [identity: unpinned]"
}

func peerToolResult(view client.Peers, pinned map[string]bool) *mcpapi.CallToolResult {
	devs, aliases, shared := view.Devices, view.Aliases, view.Shared
	identity := map[string]string{}
	for name, ok := range pinned {
		if ok {
			identity[name] = "pinned"
		} else {
			identity[name] = "unpinned"
		}
	}
	structured := map[string]any{"devices": devs, "aliases": aliases, "identity": identity}
	if view.Namespace != "" {
		structured["namespace"] = view.Namespace
	}
	if len(shared) > 0 {
		structured["shared"] = shared
	}
	if len(devs) == 0 && len(shared) == 0 {
		result := mcpapi.NewToolResultText("no devices online for this token")
		result.StructuredContent = structured
		return result
	}
	out := ""
	anyUnpinned := false
	if len(devs) > 0 {
		out = "online devices:\n"
		for _, d := range devs {
			name := canonicalTarget(view.Namespace, d)
			line := "  " + d
			if alias := aliases[d]; alias != "" {
				line += "  (" + alias + ")"
			}
			out += line + identityNote(pinned, name) + "\n"
			anyUnpinned = anyUnpinned || !pinned[name]
		}
	}
	// A device someone shared with you is only reachable as owner/device; the
	// bare label is looked up in your own namespace first.
	if len(shared) > 0 {
		out += "shared with you (use the whole owner/device as target):\n"
		for _, s := range shared {
			line := "  " + s.Target
			if s.Label != "" && s.Label != s.Device {
				line += "  (" + s.Label + ")"
			}
			if !s.Online {
				line += "  [offline]"
			}
			out += line + identityNote(pinned, s.Target) + "\n"
			anyUnpinned = anyUnpinned || !pinned[s.Target]
		}
	}
	if anyUnpinned {
		out += "\nidentity: unpinned means this session has not confirmed that device's identity yet. " +
			"The first call that dials it returns DEVICE IDENTITY CONFIRMATION REQUIRED. " +
			"wanctl_trust_server records a first-contact pin; honor the user's authorization and the host's approval requirements before changing trust.\n"
	}
	result := mcpapi.NewToolResultText(out)
	result.StructuredContent = structured
	return result
}

func mcpPair(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	target := reqStr(req, "target", "")
	if target == "" {
		return mcpapi.NewToolResultError("target is required"), nil
	}
	sess := sessions.get(ctx)
	c, hint := sess.client()
	if hint != nil {
		return hint, nil
	}
	trusted, pairingURL, err := c.Pair(ctx, target)
	if err != nil {
		// Could be: first contact, identity mismatch, target offline, relay
		// 404, token bad. dialErrorResult picks the actionable wording for the
		// first two and passes the rest through.
		return dialErrorResult(sess, err), nil
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
	if reqStr(req, "workspace", "") != "" {
		return mcpWorkspaceExec(ctx, req, false)
	}
	if _, _, hint := workspaceRoute(req); hint != nil {
		return hint, nil
	}
	target := reqStr(req, "target", "")
	command, errRes := execSource(req)
	if errRes != nil {
		return errRes, nil
	}
	sess := sessions.get(ctx)
	c, hint := sess.client()
	if hint != nil {
		return hint, nil
	}
	// Only what can be returned is kept, as it arrives; see outputKeeper.
	stdout, stderr := keepTail(), keepEnds()
	res, err := c.ExecOut(ctx, client.ExecRequest{
		Target:  target,
		Command: command,
		OneShot: reqBool(req, "oneshot"),
		Cwd:     reqStr(req, "cwd", ""),
		Elevate: reqBool(req, "elevate"),
		Via:     reqStr(req, "via", ""),
		// Ask the device to keep the whole output once it passes what can be
		// returned here. It costs a file on the device and nothing on the wire,
		// and it is the difference between "the error was in the middle of 3 MB
		// you cannot see" and one grep.
		SpillAfter: maxExecStream,
	}, stdout, stderr)
	if err != nil {
		hint := dialErrorResult(sess, err)
		if res.SpillPath != "" {
			// The command ran and failed; its output is still on the device.
			hint = mcpapi.NewToolResultError(errorTextOf(hint) + "\n" + spillNote(res))
		}
		return hint, nil
	}
	code := res.Code
	stdoutText, stderrText := stdout.tailText(res), stderr.clampText()
	out := fmt.Sprintf("exit: %d\n", code)
	if stdout.total > 0 {
		s := stdoutText
		out += "\n--- stdout ---\n" + s
		if !strings.HasSuffix(s, "\n") {
			out += "\n"
		}
	}
	if stderr.total > 0 {
		s := stderrText
		out += "\n--- stderr ---\n" + s
		if !strings.HasSuffix(s, "\n") {
			out += "\n"
		}
	}
	if stdout.total == 0 && stderr.total == 0 {
		out += "(no output)\n"
	}
	data := map[string]any{
		"target": target, "done": true, "code": code,
		"stdout": stdoutText, "stderr": stderrText,
		"stdout_truncated": stdout.total > maxExecStream, "stderr_truncated": stderr.total > maxExecStream,
	}
	if res.SpillPath != "" {
		data["spill_path"] = res.SpillPath
	}
	if res.SpillBytes > 0 {
		data["spill_bytes"], data["spill_kept"] = res.SpillBytes, res.SpillKept
	}
	return structuredResult(out, data), nil
}

// errorTextOf pulls the message back out of a tool result so a caller can add
// to it. mcp-go has no accessor, and rebuilding the result from its parts would
// drop whatever the shared dial-error wording decided to say.
func errorTextOf(res *mcpapi.CallToolResult) string {
	for _, c := range res.Content {
		if t, ok := c.(mcpapi.TextContent); ok {
			return t.Text
		}
	}
	return ""
}

// mcpRead and mcpEdit are available over the hosted HTTP endpoint as well as
// stdio, unlike wanctl_push/wanctl_pull. Those two are stdio-only because they
// name a path on the MCP server's own disk, which on a shared server belongs to
// somebody else; read and edit name a path on the target device, which is the
// thing the caller was authorized to drive in the first place.
func mcpRead(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	target, workspaceID, routeError := workspaceRoute(req)
	if routeError != nil {
		return routeError, nil
	}
	path := reqStr(req, "path", "")
	if path == "" {
		return mcpapi.NewToolResultError("path is required"), nil
	}
	sess := sessions.get(ctx)
	c, hint := workspaceClient(ctx, sess)
	if hint != nil {
		return hint, nil
	}
	res, err := c.ReadFile(ctx, client.ReadRequest{
		Target:      target,
		WorkspaceID: workspaceID,
		Path:        path,
		Offset:      reqInt(req, "offset"),
		Limit:       reqInt(req, "limit"),
	})
	if err != nil {
		return fileOpErrorResult(sess, err), nil
	}
	head := fmt.Sprintf("%s\nlines %d-%d of %d, %d bytes, sha256 %s\n",
		path, res.FirstLine, res.LastLine, res.TotalLines, res.SizeBytes, res.SHA256)
	if res.TotalLines == 0 {
		head = fmt.Sprintf("%s\nempty file, 0 bytes, sha256 %s\n", path, res.SHA256)
	}
	switch {
	case res.LongLine != 0:
		// Telling the model to continue past this line would send it back for
		// the same line every time, because the line itself is the thing that
		// does not fit.
		head += fmt.Sprintf("TRUNCATED: line %d is larger than the 256 KiB cap on its own, so only its first part is above. "+
			"Do NOT call this tool again for that line — read it with wanctl_exec (sed/cut) instead.\n", res.LongLine)
	case res.Truncated:
		head += fmt.Sprintf("TRUNCATED at the 256 KiB cap after a whole number of lines: call again with offset=%d for the rest.\n", res.LastLine+1)
	}
	data := map[string]any{
		"path": path, "content": res.Content, "first_line": res.FirstLine, "last_line": res.LastLine,
		"total_lines": res.TotalLines, "size_bytes": res.SizeBytes, "sha256": res.SHA256,
		"truncated": res.Truncated, "long_line": res.LongLine,
	}
	if res.Truncated && res.LongLine == 0 {
		data["next_offset"] = res.LastLine + 1
	}
	return structuredResult(head+"\n--- content ---\n"+res.Content, data), nil
}

func mcpEdit(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	target, workspaceID, routeError := workspaceRoute(req)
	if routeError != nil {
		return routeError, nil
	}
	path := reqStr(req, "path", "")
	old := reqStr(req, "old", "")
	edits, errRes := reqEdits(req)
	if errRes != nil {
		return errRes, nil
	}
	switch {
	case path == "":
		return mcpapi.NewToolResultError("path is required"), nil
	case len(edits) > 0 && old != "":
		return mcpapi.NewToolResultError("pass either 'old'/'new' or 'edits', not both"), nil
	case len(edits) == 0 && old == "":
		return mcpapi.NewToolResultError("pass 'old' (non-empty) with 'new', or an 'edits' array of {old,new}"), nil
	}
	sess := sessions.get(ctx)
	c, hint := workspaceClient(ctx, sess)
	if hint != nil {
		return hint, nil
	}
	request := client.EditRequest{
		Target:      target,
		WorkspaceID: workspaceID,
		Path:        path,
		All:         reqBool(req, "all"),
		ExpectedSHA: reqStr(req, "expected_sha256", ""),
		Edits:       edits,
	}
	if len(edits) == 0 {
		request.Old, request.New = old, reqStr(req, "new", "")
	}
	res, err := c.EditFile(ctx, request)
	if err != nil {
		return fileOpErrorResult(sess, err), nil
	}
	return structuredResult(fmt.Sprintf(
		"replaced %d occurrence(s) in %s\nnew sha256 %s, %d bytes\n", res.Replaced, path, res.SHA256, res.SizeBytes),
		map[string]any{"path": path, "replaced": res.Replaced, "sha256": res.SHA256, "size_bytes": res.SizeBytes}), nil
}

// reqEdits reads the batch form of the edit argument. A malformed entry is
// refused by position, because "edits is invalid" tells a caller holding six of
// them nothing about which one to fix.
func reqEdits(req mcpapi.CallToolRequest) ([]protocol.FileEdit, *mcpapi.CallToolResult) {
	raw, ok := req.GetArguments()["edits"]
	if !ok || raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, mcpapi.NewToolResultError("'edits' must be an array of {old, new} objects")
	}
	out := make([]protocol.FileEdit, 0, len(list))
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, mcpapi.NewToolResultErrorf("edits[%d] must be an object with 'old' and 'new'", i)
		}
		oldText, _ := m["old"].(string)
		newText, _ := m["new"].(string)
		if oldText == "" {
			return nil, mcpapi.NewToolResultErrorf("edits[%d]: 'old' must be a non-empty string", i)
		}
		out = append(out, protocol.FileEdit{Old: oldText, New: newText})
	}
	if len(out) == 0 {
		return nil, mcpapi.NewToolResultError("'edits' is empty; pass at least one {old, new}")
	}
	return out, nil
}

// mcpWrite creates a file on the device or replaces one end to end. It is the
// third file primitive and the one that makes the other two enough: read to
// look, edit to change, write to put a whole file there — with no base64, no
// local temp file and no heredoc through a shell.
func mcpWrite(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	target, workspaceID, routeError := workspaceRoute(req)
	if routeError != nil {
		return routeError, nil
	}
	path := reqStr(req, "path", "")
	content, given := req.GetArguments()["content"].(string)
	if path == "" {
		return mcpapi.NewToolResultError("path is required"), nil
	}
	if !given {
		// An empty string is a legitimate write — it truncates the file — so
		// "missing" and "empty" must not be the same thing here.
		return mcpapi.NewToolResultError("content is required (pass an empty string to write an empty file)"), nil
	}
	sess := sessions.get(ctx)
	c, hint := workspaceClient(ctx, sess)
	if hint != nil {
		return hint, nil
	}
	res, err := c.WriteFile(ctx, client.WriteRequest{
		Target:      target,
		WorkspaceID: workspaceID,
		Path:        path,
		Content:     content,
	})
	if err != nil {
		return fileOpErrorResult(sess, err), nil
	}
	verb := "overwrote"
	if res.Created {
		verb = "created"
	}
	return structuredResult(fmt.Sprintf(
		"%s %s\nsha256 %s, %d bytes\n", verb, path, res.SHA256, res.SizeBytes),
		map[string]any{"path": path, "created": res.Created, "sha256": res.SHA256, "size_bytes": res.SizeBytes}), nil
}

// mcpScreenshot returns what is on the device's screen as image content, which
// is the only form of it a model can actually look at. The CLI writes a file
// instead; both ask the device the same thing.
func mcpScreenshot(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	sess := sessions.get(ctx)
	c, hint := sess.client()
	if hint != nil {
		return hint, nil
	}
	var png, stderr bytes.Buffer
	res, err := c.ExecOut(ctx, client.ExecRequest{
		Target: reqStr(req, "target", ""), Command: "screenshot", OneShot: true,
		// Asked for elevated because Android cannot capture any other way and
		// only the device knows which kind it is; a desktop gates it as the
		// ordinary command it is. Optional because a desktop agent honours the
		// request with no channel to report back.
		Elevate: true, ElevateOptional: true, Via: reqStr(req, "via", ""),
	}, &png, &stderr)
	if err != nil {
		return dialErrorResult(sess, err), nil
	}
	if res.Code != 0 || png.Len() == 0 {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = fmt.Sprintf("the capture exited %d with no output", res.Code)
		}
		return mcpapi.NewToolResultError("screenshot failed on the device: " + msg), nil
	}
	return screenshotResult(png.Bytes()), nil
}

// screenshotResult turns the captured bytes into what the caller sees: image
// content it can actually look at, and one line of text saying what it is
// looking at. Separate from the call so the shaping is testable without a
// device on the other end.
func screenshotResult(raw []byte) *mcpapi.CallToolResult {
	data, mime, w, h, note, err := fitImage(raw, maxImageBytes)
	if err != nil {
		// Bytes that are not a PNG mean the verb did not run — the likeliest
		// cause by far is an agent from before desktop capture existed.
		return mcpapi.NewToolResultError(err.Error() +
			". If this device is not Android, its agent may predate desktop screenshots; run `wanctl update` on it")
	}
	line := fmt.Sprintf("%dx%d, %d bytes, %s", w, h, len(data), mime)
	if note != "" {
		line += " (" + note + ")"
	}
	return mcpapi.NewToolResultImage(line, base64.StdEncoding.EncodeToString(data), mime)
}

// fileOpErrorResult adds the two failures the file tools have that no other
// tool does — the device is running an agent too old to know these operations,
// and the answer to a request that did reach the device never came back — and
// otherwise defers to the shared dial-error wording.
func fileOpErrorResult(sess sessionAPI, err error) *mcpapi.CallToolResult {
	var unsupported *client.UnsupportedError
	if errors.As(err, &unsupported) {
		return mcpapi.NewToolResultError(err.Error() +
			". Until then, read the file with wanctl_exec (cat / Get-Content) and patch it with wanctl_push_blob.")
	}
	// The one outcome that is neither success nor failure: the request reached
	// the device and the answer did not come back. A model told "it failed"
	// would redo an edit that may already be applied, so this says what is
	// actually known and what to do about it — look first.
	var lost *client.ResultLostError
	if errors.As(err, &lost) {
		// A lost READ left nothing behind to inspect, and the error already
		// says to retry. Appending "do not simply retry, read the file first"
		// there would contradict it and send the caller after a hash of
		// something it never changed.
		if lost.Kind == protocol.KindFileRead {
			return mcpapi.NewToolResultError(err.Error())
		}
		return mcpapi.NewToolResultError(err.Error() +
			". Do NOT simply retry: call wanctl_read first and compare the sha256 against what you expected.")
	}
	return dialErrorResult(sess, err)
}

func mcpExecAsync(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	if reqStr(req, "workspace", "") != "" {
		return mcpWorkspaceExec(ctx, req, true)
	}
	if _, _, hint := workspaceRoute(req); hint != nil {
		return hint, nil
	}
	target := reqStr(req, "target", "")
	command := reqStr(req, "command", "")
	if command == "" {
		return mcpapi.NewToolResultError("command is required"), nil
	}
	sess := sessions.get(ctx)
	c, hint := sess.client()
	if hint != nil {
		return hint, nil
	}
	id, err := c.ExecAsync(ctx, target, command, reqStr(req, "cwd", ""))
	if err != nil {
		return dialErrorResult(sess, err), nil
	}
	return structuredResult(fmt.Sprintf(
		"started background job %s on %q.\nPoll it with wanctl_exec_poll(target=%q, job_id=%q) until state is 'done'. The job runs for at most 30 minutes and retains at most 8 MiB output; finished results remain available for up to 1h subject to device-wide retention budgets.",
		id, target, target, id), map[string]any{"target": target, "job_id": id}), nil
}

func mcpExecPoll(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	if reqStr(req, "workspace", "") != "" {
		return mcpWorkspacePoll(ctx, req)
	}
	if _, _, hint := workspaceRoute(req); hint != nil {
		return hint, nil
	}
	target := reqStr(req, "target", "")
	jobID := reqStr(req, "job_id", "")
	if jobID == "" {
		return mcpapi.NewToolResultError("job_id is required"), nil
	}
	sess := sessions.get(ctx)
	c, hint := sess.client()
	if hint != nil {
		return hint, nil
	}
	buf := keepEnds()
	newOffset, running, code, err := c.ExecPollTo(ctx, target, jobID, int64(reqInt(req, "offset")), buf)
	if err != nil {
		return dialErrorResult(sess, err), nil
	}
	head := fmt.Sprintf("state: running\nnext_offset: %d\n", newOffset)
	if !running {
		head = fmt.Sprintf("state: done\nexit: %d\nnext_offset: %d\n", code, newOffset)
	}
	out := head
	if buf.total > 0 {
		out += "\n--- new output ---\n" + buf.clampText()
	} else {
		out += "\n(no new output since offset)\n"
	}
	state := "running"
	if !running {
		state = "done"
	}
	data := map[string]any{
		"target": target, "job_id": jobID, "state": state, "done": !running,
		"output": buf.clampText(), "next_offset": newOffset, "truncated": buf.total > maxExecStream,
	}
	if !running {
		data["code"] = code
	}
	return structuredResult(out, data), nil
}

func mcpPush(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	target := reqStr(req, "target", "")
	local := reqStr(req, "local", "")
	remote := reqStr(req, "remote", "")
	if local == "" || remote == "" {
		return mcpapi.NewToolResultError("local and remote are required"), nil
	}
	sess := sessions.get(ctx)
	local, hint := sess.localFile(local, false)
	if hint != nil {
		return hint, nil
	}
	c, hint := sess.client()
	if hint != nil {
		return hint, nil
	}
	if err := c.Push(ctx, target, local, remote); err != nil {
		return dialErrorResult(sess, err), nil
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
	sess := sessions.get(ctx)
	c, hint := sess.client()
	if hint != nil {
		return hint, nil
	}
	if err := c.PushBytes(ctx, target, remote, data, mode); err != nil {
		return dialErrorResult(sess, err), nil
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
	sess := sessions.get(ctx)
	local, hint := sess.localFile(local, true)
	if hint != nil {
		return hint, nil
	}
	c, hint := sess.client()
	if hint != nil {
		return hint, nil
	}
	if err := c.Pull(ctx, target, remote, local); err != nil {
		return dialErrorResult(sess, err), nil
	}
	return mcpapi.NewToolResultText(fmt.Sprintf("downloaded %s:%s -> %s", target, remote, local)), nil
}

func mcpLogs(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	target := reqStr(req, "target", "")
	if target == "" {
		return mcpapi.NewToolResultError("target is required"), nil
	}
	sess := sessions.get(ctx)
	c, hint := sess.client()
	if hint != nil {
		return hint, nil
	}
	var buf bytes.Buffer
	if err := c.LogsTo(ctx, target, reqStr(req, "type", ""), reqStr(req, "grep", ""), reqStr(req, "since", ""), reqInt(req, "limit"), &buf); err != nil {
		return dialErrorResult(sess, err), nil
	}
	if buf.Len() == 0 {
		return mcpapi.NewToolResultText("(no matching events)"), nil
	}
	return mcpapi.NewToolResultText(buf.String()), nil
}

func mcpServerLogs(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	service := reqStr(req, "service", "")
	if service != "portal" && service != "relay" {
		return mcpapi.NewToolResultError("service must be portal or relay"), nil
	}
	if _, ok := sessions.get(ctx).(*remoteSession); ok {
		// On a shared server the admin secret in this process's environment
		// belongs to the operator, not to whichever tenant is logged in
		// (audit 2026-08-28, SEC-E-03).
		return mcpapi.NewToolResultError("wanctl_server_logs is available in stdio mode only: on a shared MCP server it would read the operator's logs with the operator's secret"), nil
	}
	if _, hint := sessions.get(ctx).client(); hint != nil {
		return hint, nil
	}
	since := serverlog.DefaultSince
	if raw := reqStr(req, "since", ""); raw != "" {
		var err error
		since, err = time.ParseDuration(raw)
		if err != nil || since < 0 {
			return mcpapi.NewToolResultError("since must be a non-negative duration such as '30m'"), nil
		}
	}
	limit := reqInt(req, "limit")
	if limit == 0 {
		limit = serverlog.DefaultLimit
	}
	if limit < 0 {
		return mcpapi.NewToolResultError("limit must be positive"), nil
	}
	limit = min(limit, serverlog.MaxLimit)
	q := serverlog.Query{Service: service, Since: since, Limit: limit, Grep: reqStr(req, "grep", "")}
	portalURL, err := config.Portal()
	if err != nil {
		return mcpapi.NewToolResultError(err.Error()), nil
	}
	resp, err := serverlog.Fetch(ctx, serverLogsHTTPClient,
		portalURL, os.Getenv("WANCTL_ADMIN_SECRET"), q)
	if err != nil {
		return mcpapi.NewToolResultError(err.Error()), nil
	}
	var out bytes.Buffer
	if err := serverlog.Format(&out, resp); err != nil {
		return mcpapi.NewToolResultError(err.Error()), nil
	}
	if out.Len() == 0 {
		return mcpapi.NewToolResultText("(no matching server log lines)"), nil
	}
	return mcpapi.NewToolResultText(out.String()), nil
}

// --- info tools (use the session's identity if any) ---

func mcpID(ctx context.Context, _ mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	s := sessions.get(ctx)
	if r, ok := s.(*remoteSession); ok {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.token == "" {
			return notAuthorized(), nil
		}
		if err := r.ensureIdentity(); err != nil {
			return mcpapi.NewToolResultError("derive identity: " + err.Error()), nil
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
	if os.Getenv("WANCTL_MCP_ALLOW_UNSAFE_TRUST_SERVER") != "1" {
		return mcpapi.NewToolResultError("device identity pinning is not enabled on this MCP server: the operator has not set WANCTL_MCP_ALLOW_UNSAFE_TRUST_SERVER=1, so this tool cannot pin anything and retrying will not help. A human has to do it from a terminal on this machine: verify it with the device owner, then run `wanctl trust server --target OWNER/DEVICE --fingerprint SHA256:... --number N --code C` (the number and the code the device prints) or `wanctl trust server --target OWNER/DEVICE --fingerprint SHA256:...`. Report that to the user, with the exact target and the values from the confirmation result, and stop"), nil
	}
	target := reqStr(req, "target", "")
	fingerprint := reqStr(req, "fingerprint", "")
	number := reqStr(req, "number", "")
	code := reqStr(req, "code", "")
	switch {
	case target == "":
		return mcpapi.NewToolResultError("target is required, copied verbatim from the DEVICE IDENTITY CONFIRMATION REQUIRED result"), nil
	case number == "" && fingerprint == "":
		return mcpapi.NewToolResultError("nothing to verify with: pass number, its original fingerprint and code read off the device, or the fingerprint the user compared. Neither is optional, and a pin with neither would record an identity nobody checked"), nil
	case number != "" && code == "":
		return mcpapi.NewToolResultError("code is required with number: it is the nine digits the user read off the device for that verification number, and it is what makes this a check rather than an assertion"), nil
	case number != "" && fingerprint == "":
		return mcpapi.NewToolResultError("fingerprint is required with number: copy target, fingerprint and number from the same first-contact refusal"), nil
	case number == "" && code != "":
		return mcpapi.NewToolResultError("number is required with code"), nil
	}
	sess := sessions.get(ctx)
	c, hint := sess.client()
	if hint != nil {
		return hint, nil
	}
	// Never replace. The identity a model "confirms" is the one the current
	// dial presented — exactly the value a hostile relay would substitute — so
	// a changed certificate must stay a hard failure here and go through a
	// human at a terminal (audit 2026-08-28, SEC-E-02).
	if number != "" {
		canonical, pinned, err := c.ConfirmTrust(ctx, &client.TrustChallenge{Target: target, Fingerprint: fingerprint, Number: number}, code, false)
		if err != nil {
			return dialErrorResult(sess, err), nil
		}
		return mcpapi.NewToolResultText(fmt.Sprintf("confirmed device identity: %s %s", canonical, pinned)), nil
	}
	canonical, err := c.PinServer(ctx, target, fingerprint, false)
	if err != nil {
		return dialErrorResult(sess, err), nil
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

// clampHead is how much of the start of a stream clampText keeps; the rest of
// maxExecStream goes to the end, where a command's result usually is.
const clampHead = 8 * 1024

// outputKeeper holds what a caller will be shown of one output stream while
// the stream is still arriving: its first headN bytes, its last tailN, and a
// count of all of it. How much a device sends is the device's choice — a
// command can print without end, and nothing obliges an agent to honour
// SpillAfter — so nothing here grows with the output. The server used to
// collect each stream whole and cut it down when the command ended, holding
// everything a device sent until then.
type outputKeeper struct {
	headN, tailN int
	head, tail   []byte
	total        int64
}

// keepTail keeps what tailText shows: the last maxExecStream bytes.
func keepTail() *outputKeeper { return &outputKeeper{tailN: maxExecStream} }

// keepEnds keeps what clampText shows: the first clampHead bytes and the last
// maxExecStream-clampHead.
func keepEnds() *outputKeeper {
	return &outputKeeper{headN: clampHead, tailN: maxExecStream - clampHead}
}

// Write never fails and keeps at most headN + 2*tailN bytes: the tail buffer
// runs to twice its size before it is shifted down, so the copy happens once
// per tailN bytes of output rather than on every write.
func (k *outputKeeper) Write(p []byte) (int, error) {
	n := len(p)
	k.total += int64(n)
	if room := k.headN - len(k.head); room > 0 {
		take := min(room, len(p))
		k.head = append(k.head, p[:take]...)
		p = p[take:]
	}
	if len(p) > k.tailN {
		p = p[len(p)-k.tailN:]
	}
	if len(k.tail)+len(p) > 2*k.tailN {
		keep := k.tailN - len(p) // with p appended, exactly tailN remain
		k.tail = append(k.tail[:0], k.tail[len(k.tail)-keep:]...)
	}
	k.tail = append(k.tail, p...)
	return n, nil
}

// last is the stream's final tailN bytes, or all of what followed the head
// when there were fewer.
func (k *outputKeeper) last() []byte {
	return k.tail[max(0, len(k.tail)-k.tailN):]
}

// tailText is what a synchronous exec returns for stdout (a keepTail keeper).
//
// It keeps the END of the output, not the beginning and the end. A command's
// result is at the end: the compiler's error summary, the test run's failures,
// the last line of a log. The head was only ever worth keeping when it was the
// only copy of anything; now that the device holds the whole output in a file,
// the head is one grep away and the tail is what the caller needs in front of
// it. deviceCopy names that file, so the next call is `grep` and not the same
// command again with a filter guessed blind.
func (k *outputKeeper) tailText(res client.ExecOutcome) string {
	if k.total <= maxExecStream {
		return string(k.head) + string(k.tail)
	}
	tail := k.last()
	// The device counted every byte it produced; this keeper counted only what
	// arrived on one stream. When the two disagree the device's number is the
	// true one, and it is the number a caller reasons about when deciding
	// whether the tail is worth reading at all.
	total := k.total
	if res.SpillBytes > total {
		total = res.SpillBytes
	}
	return fmt.Sprintf("[output truncated: showing last %d of %d bytes; %s]\n",
		len(tail), total, spillNote(res)) + string(tail)
}

// spillNote says what became of the rest of the output. There are three
// answers, and a caller acts differently on each: the device has it, the device
// tried and could not, or the device was never asked because its agent is too
// old to know how. The middle case is why the device reports the byte count
// even when it reports no path — silence alone cannot tell the last two apart.
func spillNote(res client.ExecOutcome) string {
	switch {
	case res.SpillPath != "" && res.SpillKept > 0 && res.SpillKept < res.SpillBytes:
		return fmt.Sprintf(
			"the device kept the FIRST %d bytes at %s (its copy is capped, so the middle is gone) — read it with wanctl_read or grep it with wanctl_exec",
			res.SpillKept, res.SpillPath)
	case res.SpillPath != "":
		return "full output at " + res.SpillPath +
			" on the device, kept for 1h — read it with wanctl_read or grep it with wanctl_exec"
	case res.SpillBytes > 0:
		// It counted, so it understood the request and the file is what failed.
		return "the device could not keep the full output (no writable temp space); narrow the command with a filter instead"
	default:
		return "the device did not keep the full output (its agent predates this; run `wanctl update` there)"
	}
}

// clampText is what stderr and polled output return (a keepEnds keeper): all
// of it when it fits, otherwise its start and its end around a line saying how
// much was cut.
func (k *outputKeeper) clampText() string {
	if k.total <= maxExecStream {
		return string(k.head) + string(k.tail)
	}
	tail := k.last()
	dropped := k.total - int64(len(k.head)) - int64(len(tail))
	var sb strings.Builder
	sb.Write(k.head)
	sb.WriteString(fmt.Sprintf(
		"\n\n[... wanctl truncated %d bytes (%d total); showing first %d + last %d. "+
			"Re-run with a tighter filter, e.g. `... | Select-Object -Last 200` or `... | tail -n 200` ...]\n\n",
		dropped, k.total, len(k.head), len(tail)))
	sb.Write(tail)
	return sb.String()
}

// Compile-time anchor for indirectly-used packages.
var _ = http.StatusOK
