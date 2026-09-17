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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"wanctl/internal/client"
	"wanctl/internal/config"
	"wanctl/internal/limits"
	"wanctl/internal/mcpauth"
	"wanctl/internal/policy"
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
	return HandlerWithOptions(Options{Seed: seed, EndpointPath: endpointPath})
}

// Options configures the hosted MCP handler.
type Options struct {
	// Seed is the relay's MCP seed (≥32 bytes).
	Seed []byte
	// EndpointPath is the public path the AI host POSTs to, usually "/mcp".
	EndpointPath string
	// OAuth, when non-nil, additionally accepts OAuth 2.1 bearer tokens.
	OAuth *OAuthConfig
}

// OAuthConfig turns on bearer authentication for the hosted endpoint.
//
// It exists because one class of client — ChatGPT's connector is the one that
// forced the issue — starts a new MCP session for every single tool call. A
// login stored against Mcp-Session-Id can never survive that, so those clients
// saw "LOGIN REQUIRED" immediately after logging in successfully. A bearer is
// carried on every request instead, so identity stops depending on the session.
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

// HandlerWithOptions is Handler with the optional pieces spelled out.
func HandlerWithOptions(o Options) (http.Handler, error) {
	if len(o.Seed) < 32 {
		return nil, fmt.Errorf("mcp seed must be at least 32 bytes, got %d", len(o.Seed))
	}
	sessions = &sessionStore{
		seed:    append([]byte(nil), o.Seed...),
		m:       map[string]*remoteSession{},
		revoked: map[string]time.Time{},
		trust:   map[string]*transport.Store{},
		oauth:   o.OAuth,
	}
	go sessions.gcLoop()
	s := server.NewMCPServer("wanctl", "1.0.0")
	registerMCPTools(s)
	opts := []server.StreamableHTTPOption{}
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
	h := refuseBrowserOrigins(server.NewStreamableHTTPServer(s, opts...))
	if o.OAuth != nil {
		h = oauthGate(append([]byte(nil), o.Seed...), o.OAuth, h)
	}
	return h, nil
}

type oauthClaimKey struct{}

func oauthClaimFrom(ctx context.Context) (mcpauth.Claim, bool) {
	claim, ok := ctx.Value(oauthClaimKey{}).(mcpauth.Claim)
	return claim, ok
}

// oauthGate authenticates the bearer before mcp-go sees the request.
//
// No Authorization header keeps the old path exactly as it was: a session keyed
// by Mcp-Session-Id, logging in through wanctl_login. Clients that hold their
// session open — Claude Code, Codex, Cursor — never notice this code exists.
func oauthGate(seed []byte, cfg *OAuthConfig, next http.Handler) http.Handler {
	challenge := `Bearer resource_metadata="` + cfg.ResourceMetadataURL + `"`
	deny := func(w http.ResponseWriter, code, description string) {
		w.Header().Set("WWW-Authenticate", challenge+`, error="`+code+`", error_description="`+description+`"`)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"` + code + `","error_description":"` + description + `"}`))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw := strings.TrimSpace(req.Header.Get("Authorization"))
		if raw == "" {
			next.ServeHTTP(w, req)
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
// otherwise reach a localhost `wanctl mcp --http` or DNS-rebind to a bound
// address and drive it (audit 2026-08-28, SEC-E-05). WANCTL_MCP_ALLOWED_ORIGINS
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
	seed    []byte
	m       map[string]*remoteSession
	revoked map[string]time.Time // process-local JTI revocations; not durable across restart

	// OAuth sessions are keyed by the access token's JTI rather than by
	// Mcp-Session-Id, which is the whole point: the same bearer reaching us in
	// two unrelated MCP sessions is one logged-in person, not two strangers.
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

// oauthSession returns the session a bearer names, already logged in. There is
// no wanctl_login step on this path: the browser trip that minted the token was
// the login, and the token carries the namespace and the relay credential.
func (s *sessionStore) oauthSession(claim mcpauth.Claim) sessionAPI {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := "oauth:" + claim.JTI
	r := s.m[key]
	if r == nil {
		r = &remoteSession{
			id: key, seed: s.seed, owner: s,
			known:      s.trustForLocked(claim.Namespace),
			rebindJTIs: map[string]time.Time{},
			oauth:      true, oauthClaim: claim,
			token: claim.Token, namespace: claim.Namespace,
		}
		s.m[key] = r
	}
	r.lastUsed = time.Now()
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

func (l *localFsSession) saveLogin(token, _ string) error { return config.SaveToken(token) }
func (l *localFsSession) clearLogin() error               { return config.ClearToken() }
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

	// Set when this session was reached with an OAuth bearer. Such a session is
	// born logged in and has no wanctl_login step.
	oauth      bool
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
		return nil, loginRequired()
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
	// On the OAuth path, forgetting the token in this process would mean
	// nothing: the bearer carries it, and the next request rebuilds the session
	// from it. Logging out has to reach the relay and kill the namespace token.
	if r.oauth {
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
	out := "mode:                  http (remote, per-MCP-session state)\n"
	if r.oauth {
		// A client on this path may open a new MCP session per call, so the
		// session id is noise. What answers "am I logged in" is the bearer.
		out = "mode:                  http (remote, OAuth bearer)\n"
		out += "client:                " + r.oauthClaim.ClientID + "\n"
		out += "access token expires:  " + r.oauthClaim.Expiry().UTC().Format(time.RFC3339) + "\n"
	} else {
		out += "session id:            " + r.id + "\n"
	}
	if r.token == "" {
		out += "status:                NOT logged in — call wanctl_login() to start\n"
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

func registerMCPTools(s *server.MCPServer) {
	s.AddTool(mcpapi.NewTool("wanctl_login",
		mcpapi.WithDescription("Authenticate THIS MCP session to a wanctl namespace via the team portal. Two-step OAuth flow: (1) call with NO argument first → returns a portal URL + a one-time code prompt the user needs to complete in their browser. (2) call again with the `code` the user pastes back → exchanges it for a namespace token bound ONLY to this MCP session (in HTTP mode) or this machine's wanctl config (in stdio mode). Multiple AI users sharing the same MCP server each log in independently — credentials are never shared across sessions.\n\nFAST RE-BIND: a successful login also returns a `rebind` credential. HTTP-MCP sessions are in-memory, so a relay restart or a dropped/re-initialized connection can surface 'LOGIN REQUIRED' mid-task even though the user is still authorized. When that happens, call wanctl_login(rebind=\"…\") with the credential you saved — it restores access INSTANTLY with no portal round-trip. Only fall back to the OAuth flow if you have no saved rebind credential."),
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
		mcpapi.WithDescription("List devices currently reachable by the active controller token. Returns each stable device ID with its display label and whether this session has pinned its identity yet ('identity: pinned' / 'identity: unpinned'); structured content contains backward-compatible devices and aliases fields plus an identity map keyed by the canonical namespace/device. Purely a local lookup — it dials nothing. Use this FIRST when the user asks 'what devices are available' or before guessing a target."),
	), mcpPeers)

	s.AddTool(mcpapi.NewTool("wanctl_pair",
		mcpapi.WithDescription("Check whether the target device already trusts this MCP session's controller identity, and if not, return the device-side pairing URL up front. On first contact this may instead return DEVICE IDENTITY CONFIRMATION REQUIRED; answer it yourself by calling wanctl_trust_server with the target and fingerprint from that result, then retry — no need to ask the user first. Once the server identity is pinned, returns '✓ already trusted' OR 'PAIRING REQUIRED' with a URL to relay VERBATIM to the user."),
		mcpapi.WithString("target", mcpapi.Required(), mcpapi.Description("Device ID or unique name/alias (DEVICE|ALIAS), or NS/DEVICE|NS/ALIAS for shared devices.")),
	), mcpPair)

	s.AddTool(mcpapi.NewTool("wanctl_exec",
		mcpapi.WithDescription("Run a shell command, or a whole script, on a remote wanctl-enrolled device over the encrypted relay. Returns the device's stdout, stderr, and exit code. Pass EITHER 'command' (a one-liner) OR 'script' (multi-line source) — prefer 'script' for anything with a $, a quote inside a quote, or more than one statement, because a script is transported encoded and is never parsed by the device's shell. If the device hasn't paired this controller yet, the result is isError=true with a 'PAIRING REQUIRED' message that carries a URL — surface that URL VERBATIM to the user; do not paraphrase. If instead it says DEVICE IDENTITY CONFIRMATION REQUIRED, that is first contact: call wanctl_trust_server with the target and fingerprint it gives you and retry, without asking the user."),
		mcpapi.WithString("target", mcpapi.Required(), mcpapi.Description("Device ID or unique name/alias (DEVICE|ALIAS), or NS/DEVICE|NS/ALIAS for shared devices. If exactly one device is online for this token, you may pass empty string.")),
		mcpapi.WithString("command", mcpapi.Description("A one-liner for the device's default shell (sh on Unix, powershell on Windows). WARNING: this string is SOURCE CODE for that shell and is parsed there. On Windows that means writing `powershell -Command \"...$x...\"` gets parsed TWICE — the outer shell expands $x to nothing and the inner script fails with a misleading 'term is not recognized'. Use 'script' instead of nesting an interpreter here.")),
		mcpapi.WithString("script", mcpapi.Description("Script SOURCE to run on the device (not a file path). Sent encoded, so quoting and character-set rules do not apply: $, backticks, nested quotes and non-ASCII text all arrive literally. Requires 'interp'. Use this for multi-statement work; it is the same single call as 'command'. Scripts over ~9KB must be pushed as a file and run by path instead.")),
		mcpapi.WithString("interp", mcpapi.Description("Interpreter for 'script': 'powershell' for Windows devices, 'sh' for Unix/macOS/Android. Required when 'script' is set.")),
		mcpapi.WithString("cwd", mcpapi.Description("Working directory on the device for this command (also the policy scope).")),
		mcpapi.WithBoolean("oneshot", mcpapi.Description("Run in a fresh shell with no persistent session state. Default false — successive exec calls share cwd/env like a real terminal.")),
		mcpapi.WithBoolean("elevate", mcpapi.Description("Android only. Run with elevated privilege (uid 0 or the adb shell uid 2000) instead of the app sandbox the agent normally lives in. This is what makes `pm`, `am`, `input`, `screencap`, `dumpsys`, `settings`, `wm` and `svc` work at all — without it they fail with permission errors or empty output. Elevated commands need their OWN policy rule on the device; a device in bypass mode still refuses them until a human approves, so expect a 'PAIRING/approval' style rejection the first time.")),
		mcpapi.WithString("via", mcpapi.Description("Pin the elevation channel: 'su' (rooted device) or 'adb' (device's own wireless debugging). Default empty = let the device pick whichever is available. Naming an unavailable channel fails instead of quietly running unprivileged.")),
	), mcpExec)

	s.AddTool(mcpapi.NewTool("wanctl_exec_async",
		mcpapi.WithDescription("Start a shell command as a BACKGROUND job on the device and return a job_id IMMEDIATELY, without waiting for it to finish. Use this for anything that may run longer than a single tool call comfortably tolerates — package installs, builds, large downloads, `wsl --shutdown` then a long build, etc. The command keeps running on the device even after this call returns; fetch its output and exit code later with wanctl_exec_poll(job_id). Always runs in a FRESH shell (no shared cwd/env with wanctl_exec's persistent session). Same pairing/policy rules as wanctl_exec. Jobs run for at most 30 minutes, retain at most 8 MiB output each, and finished results remain pollable for up to 1h subject to device-wide retention budgets."),
		mcpapi.WithString("target", mcpapi.Required(), mcpapi.Description("Device ID or unique name/alias (DEVICE|ALIAS), or NS/DEVICE|NS/ALIAS.")),
		mcpapi.WithString("command", mcpapi.Required(), mcpapi.Description("Shell command to run in the device's default shell (sh on Unix, powershell on Windows).")),
		mcpapi.WithString("cwd", mcpapi.Description("Working directory on the device for this command (also the policy scope).")),
	), mcpExecAsync)

	s.AddTool(mcpapi.NewTool("wanctl_exec_poll",
		mcpapi.WithDescription("Fetch a background job's new output and status (started via wanctl_exec_async). Call repeatedly until state is 'done'. Pass the 'next_offset' from the previous poll as 'offset' to receive only NEW output each time; omit or 0 to get everything from the start. The response carries a status header (state: running|done, exit code when done, next_offset) followed by the output."),
		mcpapi.WithString("target", mcpapi.Required(), mcpapi.Description("Device ID or unique name/alias (DEVICE|ALIAS), or NS/DEVICE|NS/ALIAS — the same device the job was started on.")),
		mcpapi.WithString("job_id", mcpapi.Required(), mcpapi.Description("The job id returned by wanctl_exec_async.")),
		mcpapi.WithNumber("offset", mcpapi.Description("Bytes of output already seen; return only output past this point. Use the previous poll's next_offset. Default 0 = from the start.")),
	), mcpExecPoll)

	s.AddTool(mcpapi.NewTool("wanctl_push",
		mcpapi.WithDescription("Upload a local file to a remote path on the target device. Same pairing/policy rules as wanctl_exec. Available in stdio mode only (on a shared HTTP MCP server 'local' would be a path on the server itself). Paths under a dot-directory of the operator's home (~/.ssh, ~/.config, …) are refused; WANCTL_MCP_LOCAL_ROOT confines the tool to one tree."),
		mcpapi.WithString("target", mcpapi.Required(), mcpapi.Description("Device ID or unique name/alias (DEVICE|ALIAS), or NS/DEVICE|NS/ALIAS.")),
		mcpapi.WithString("local", mcpapi.Required(), mcpapi.Description("Absolute path on the MCP-server machine (or your local machine in stdio mode) to upload.")),
		mcpapi.WithString("remote", mcpapi.Required(), mcpapi.Description("Absolute path on the target device to write to.")),
	), mcpPush)

	s.AddTool(mcpapi.NewTool("wanctl_push_blob",
		mcpapi.WithDescription("Upload INLINE base64 content to a remote path on the target device — the file-push tool that works in HTTP (remote) MCP mode, where the AI host has no file on the MCP server for wanctl_push to read. Encode the bytes you want written as base64 and pass them in 'content_b64'. Same pairing/policy rules as wanctl_exec. Size cap: 8 MiB of raw (decoded) bytes; for larger payloads, split or have the device fetch the file itself."),
		mcpapi.WithString("target", mcpapi.Required(), mcpapi.Description("Device ID or unique name/alias (DEVICE|ALIAS), or NS/DEVICE|NS/ALIAS.")),
		mcpapi.WithString("remote", mcpapi.Required(), mcpapi.Description("Absolute path on the target device to write to (overwrites if it exists).")),
		mcpapi.WithString("content_b64", mcpapi.Required(), mcpapi.Description("Standard-base64-encoded file content (the RAW bytes to write, not text).")),
		mcpapi.WithString("mode", mcpapi.Description("Optional octal file mode, e.g. \"0755\" for an executable. Default 0644.")),
	), mcpPushBlob)

	s.AddTool(mcpapi.NewTool("wanctl_pull",
		mcpapi.WithDescription("Download a remote file from the target device to a local path. Same pairing/policy rules as wanctl_exec. Available in stdio mode only; the same local-path limits as wanctl_push apply. On a shared HTTP MCP server read remote files with wanctl_exec instead."),
		mcpapi.WithString("target", mcpapi.Required(), mcpapi.Description("Device ID or unique name/alias (DEVICE|ALIAS), or NS/DEVICE|NS/ALIAS.")),
		mcpapi.WithString("remote", mcpapi.Required(), mcpapi.Description("Absolute path on the target device to read.")),
		mcpapi.WithString("local", mcpapi.Required(), mcpapi.Description("Absolute path on the MCP-server machine (or your local machine in stdio mode) to write to.")),
	), mcpPull)

	s.AddTool(mcpapi.NewTool("wanctl_logs",
		mcpapi.WithDescription("Pull JSONL activity events from the target device's local log (every connect/exec/file with its decision and exit code). Useful for auditing what happened, including past pairing/approval outcomes."),
		mcpapi.WithString("target", mcpapi.Required(), mcpapi.Description("Device ID or unique name/alias (DEVICE|ALIAS), or NS/DEVICE|NS/ALIAS.")),
		mcpapi.WithString("type", mcpapi.Description("Filter: 'connect', 'exec', or 'file'.")),
		mcpapi.WithString("grep", mcpapi.Description("Filter: substring of the detail field.")),
		mcpapi.WithString("since", mcpapi.Description("Filter: RFC3339 timestamp lower bound.")),
		mcpapi.WithNumber("limit", mcpapi.Description("Return at most this many of the most recent matching events (0 = no cap).")),
	), mcpLogs)

	s.AddTool(mcpapi.NewTool("wanctl_server_logs",
		mcpapi.WithDescription("Read recent portal or relay process logs through the secret-gated admin API. Output is redacted before filtering and bounded by the requested limit."),
		mcpapi.WithString("service", mcpapi.Required(), mcpapi.Description("Server service: 'portal' or 'relay'.")),
		mcpapi.WithString("since", mcpapi.Description("Lookback duration such as '30m' or '2h'. Default 15m.")),
		mcpapi.WithNumber("limit", mcpapi.Description("Return at most this many recent lines. Default 200, maximum 2000.")),
		mcpapi.WithString("grep", mcpapi.Description("Filter by substring after credential redaction.")),
	), mcpServerLogs)

	s.AddTool(mcpapi.NewTool("wanctl_id",
		mcpapi.WithDescription("Show THIS MCP session's controller identity fingerprint. The fingerprint is what target devices pair against in the trust step."),
	), mcpID)

	s.AddTool(mcpapi.NewTool("wanctl_trust",
		mcpapi.WithDescription("List the trust store for THIS MCP session. 'servers' (default) = explicitly pinned devices. 'clients' = controllers this machine has trusted to drive it (only meaningful in stdio mode if this machine is also running wanctl agent)."),
		mcpapi.WithString("which", mcpapi.Description("'servers' (default) or 'clients'.")),
	), mcpTrust)

	s.AddTool(mcpapi.NewTool("wanctl_trust_server",
		mcpapi.WithDescription("Pin a device's identity for THIS session. Call it as soon as any tool returns DEVICE IDENTITY CONFIRMATION REQUIRED, passing the target and fingerprint copied VERBATIM from that result, then retry the call that failed — do not ask the user to confirm the fingerprint first, because your MCP client's own approval prompt is already the human checkpoint. This is a first-contact step only: it records what the device presented right now, so later calls can detect a change. If a call instead returns 'DEVICE IDENTITY MISMATCH', do NOT call this tool — report both the pinned and the presented fingerprint to the user and stop; re-pinning is a human's decision at a terminal. Note: the handler refuses unless the operator has set WANCTL_MCP_ALLOW_UNSAFE_TRUST_SERVER=1 (the hosted endpoint does; a local stdio server usually does not, and there a human runs `wanctl trust server` instead)."),
		mcpapi.WithString("target", mcpapi.Required(), mcpapi.Description("The owner/device target, copied verbatim from the DEVICE IDENTITY CONFIRMATION REQUIRED result.")),
		mcpapi.WithString("fingerprint", mcpapi.Required(), mcpapi.Description("The SHA256:... fingerprint, copied verbatim from the same result.")),
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

// trustRequiredResult is the first-contact message, written for a model.
//
// The CLI text it replaces tells the reader to run `wanctl trust server`, a
// command that does not exist on the MCP surface. Observed 2026-09-17: the
// model could not map it to a tool, so it asked the user to "confirm the
// fingerprint on the device" for two turns and only called wanctl_trust_server
// after the user suggested it. The pin is worth keeping -- its value is the
// mismatch alarm later -- but the step has to be one the model can take itself.
func trustRequiredResult(e *client.TrustRequiredError) *mcpapi.CallToolResult {
	return mcpapi.NewToolResultError(fmt.Sprintf(
		"DEVICE IDENTITY CONFIRMATION REQUIRED. This session has not pinned %q yet, so this was first contact and nothing was sent.\n"+
			"  target:      %s\n"+
			"  fingerprint: %s\n\n"+
			"DO THIS NOW, without asking the user first: call wanctl_trust_server with target=%q and fingerprint=%q, copying both values verbatim from the two lines above, then retry the call you just made.\n\n"+
			"Do not ask the user to confirm the fingerprint in chat: your MCP client already asks them to approve each tool call, and that prompt is the human checkpoint. This happens once per device. From then on a changed identity fails closed and comes back as DEVICE IDENTITY MISMATCH, which is what the pin is for.",
		e.Target, e.Target, e.Fingerprint, e.Target, e.Fingerprint,
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

func loginRequired() *mcpapi.CallToolResult {
	return mcpapi.NewToolResultError(
		"LOGIN REQUIRED. This MCP session has no wanctl credentials right now.\n" +
			"FIRST: if earlier in THIS conversation a wanctl_login succeeded and returned a `rebind` credential, the user is almost certainly still authorized — the in-memory session was just lost (relay restart / reconnect). Call wanctl_login(rebind=\"…\") with that saved credential to restore access INSTANTLY; do NOT bother the user. " +
			"ONLY if you have no saved rebind credential: call wanctl_login() (no args) — it returns a URL + instructions to show the user, who signs in to the portal and pastes back a one-time code for wanctl_login(code=\"…\"). Then retry your previous tool call.",
	)
}

// --- auth tools ---

func mcpLogin(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	s := sessions.get(ctx)
	// An OAuth bearer already is the login: the user did the browser trip when
	// they authorized this client. Sending them to /enroll for a one-time code
	// would be asking them to log in a second time to the same account.
	if r, ok := s.(*remoteSession); ok && r.oauth {
		return mcpapi.NewToolResultText(fmt.Sprintf(
			"✓ 本连接已通过 OAuth 登录为 namespace %q，不需要再走 /enroll 取 code。直接调 wanctl_peers / wanctl_exec 即可。\n"+
				"（授权是跟着这个连接器的访问令牌走的，不依赖 MCP 会话；要换账号或收回授权，去门户的访问令牌页吊销，或调 wanctl_logout。）",
			r.namespace)), nil
	}
	code := reqStr(req, "code", "")
	rebind := reqStr(req, "rebind", "")
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
		relayURL, err := s.relayURL()
		if err != nil {
			return mcpapi.NewToolResultError(err.Error()), nil
		}
		realNS, err := client.ResolveTokenNamespace(ctx, relayURL, claim.Token, tr)
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
	token, ns := en.Token, en.Namespace
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
			"\n\n⚠️ 给 AI 自己记(不必展示给用户): 保存下面这串 rebind 凭证。之后任何工具若返回 'LOGIN REQUIRED'(relay 重启/连接重置会触发,但用户其实仍有授权), 不要再打扰用户重新走浏览器登录 —— 直接调 wanctl_login(rebind=\"…\") 即可瞬时恢复:\n\n  %s",
			credential)
	}
	return mcpapi.NewToolResultText(msg), nil
}

func mcpStatus(ctx context.Context, _ mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	return mcpapi.NewToolResultText(sessions.get(ctx).info()), nil
}

func mcpLogout(ctx context.Context, _ mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	s := sessions.get(ctx)
	oauth := false
	if r, ok := s.(*remoteSession); ok {
		oauth = r.oauth
	}
	if err := s.clearLogin(); err != nil {
		return mcpapi.NewToolResultError(err.Error()), nil
	}
	if oauth {
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
			"The first call that dials it returns DEVICE IDENTITY CONFIRMATION REQUIRED; answer that by calling " +
			"wanctl_trust_server with the target and fingerprint it hands you, then retry. No need to ask the user first.\n"
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
	var stdout, stderr bytes.Buffer
	code, err := c.ExecTo(ctx, client.ExecRequest{
		Target:  target,
		Command: command,
		OneShot: reqBool(req, "oneshot"),
		Cwd:     reqStr(req, "cwd", ""),
		Elevate: reqBool(req, "elevate"),
		Via:     reqStr(req, "via", ""),
	}, &stdout, &stderr)
	if err != nil {
		return dialErrorResult(sess, err), nil
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
	sess := sessions.get(ctx)
	c, hint := sess.client()
	if hint != nil {
		return hint, nil
	}
	id, err := c.ExecAsync(ctx, target, command, reqStr(req, "cwd", ""))
	if err != nil {
		return dialErrorResult(sess, err), nil
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
	sess := sessions.get(ctx)
	c, hint := sess.client()
	if hint != nil {
		return hint, nil
	}
	var buf bytes.Buffer
	newOffset, running, code, err := c.ExecPollTo(ctx, target, jobID, int64(reqInt(req, "offset")), &buf)
	if err != nil {
		return dialErrorResult(sess, err), nil
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
	if os.Getenv("WANCTL_MCP_ALLOW_UNSAFE_TRUST_SERVER") != "1" {
		return mcpapi.NewToolResultError("device identity pinning is not enabled on this MCP server: the operator has not set WANCTL_MCP_ALLOW_UNSAFE_TRUST_SERVER=1, so this tool cannot pin anything and retrying will not help. A human has to do it from a terminal on this machine: verify the fingerprint with the device owner, then run `wanctl trust server --target OWNER/DEVICE --fingerprint SHA256:...`. Report that to the user, with the exact target and fingerprint from the confirmation result, and stop"), nil
	}
	target := reqStr(req, "target", "")
	fingerprint := reqStr(req, "fingerprint", "")
	if target == "" || fingerprint == "" {
		return mcpapi.NewToolResultError("target and fingerprint are required"), nil
	}
	sess := sessions.get(ctx)
	c, hint := sess.client()
	if hint != nil {
		return hint, nil
	}
	// Never replace. The fingerprint a model "confirms" is the one the
	// current dial presented — exactly the value a hostile relay would
	// substitute — so a changed certificate must stay a hard failure here and
	// go through a human at a terminal (audit 2026-08-28, SEC-E-02).
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
