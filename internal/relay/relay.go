// Package relay is the public WebSocket broker. It authenticates endpoints by
// token, keeps an in-memory registry of online devices, and byte-pipes a
// controller's session to the matching agent. It never terminates or inspects
// the end-to-end TLS that flows through it.
package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"wanctl/internal/admission"
	"wanctl/internal/limits"
	"wanctl/internal/notify"
	"wanctl/internal/serverlog"
	"wanctl/internal/sessionauth"
	"wanctl/internal/wsconn"

	"github.com/coder/websocket"
)

type agentConn struct {
	device string
	ns     string
	inst   string
	ctrl   io.Writer
	mu     sync.Mutex // serialize control writes
}

func (a *agentConn) send(v any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return json.NewEncoder(a.ctrl).Encode(v)
}

type pendingSession struct {
	agentSide chan io.ReadWriteCloser
	done      chan struct{}
}

// ACLChecker returns raw permissions for a live cross-namespace grant. Relay
// parses them strictly before opening a capability-scoped session.
type ACLChecker interface {
	ACLPerms(callerNS, targetNS, device string) (string, bool)
}

// Auditor records relay-side metadata events (nil = no audit).
type Auditor interface {
	Audit(namespace, device, event string)
}

type webhookSender interface {
	Send(context.Context, notify.Destination, notify.Event) (notify.Result, error)
}

// Relay is the broker.
type Relay struct {
	ts          TokenStore
	acl         ACLChecker
	audit       Auditor
	admin       AdminStore
	aliases     DeviceAliasStore
	notifyStore NotifyStore
	notifySend  webhookSender
	docs        DocsStore
	mcpHandler  http.Handler // optional: HTTP/Streamable MCP at /mcp
	adminSecret string
	portalNS    string
	logs        *serverlog.Buffer

	mu      sync.Mutex
	agents  map[string]*agentConn      // key "ns/device" (WebSocket transport)
	pending map[string]*pendingSession // key session id (WebSocket transport)

	hmu        sync.Mutex
	hagents    map[string]*httpAgent   // key "ns/device" (HTTP transport)
	hsess      map[string]*httpSession // key session id (HTTP transport)
	reaperOnce sync.Once

	enrollMu    sync.Mutex
	enrollCodes map[string]*enrollCode // one-time device-enrollment codes

	notifyDedupeMu sync.Mutex
	notifyDedupe   map[notifyDedupeKey]time.Time
}

// New constructs a Relay backed by the given TokenStore.
func New(ts TokenStore) *Relay {
	return &Relay{
		ts:           ts,
		agents:       map[string]*agentConn{},
		pending:      map[string]*pendingSession{},
		hagents:      map[string]*httpAgent{},
		hsess:        map[string]*httpSession{},
		enrollCodes:  map[string]*enrollCode{},
		notifyDedupe: map[notifyDedupeKey]time.Time{},
		notifySend:   notify.NewSender(notify.Options{}),
	}
}

// Handler returns the relay's HTTP mux.
func (r *Relay) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/agent", r.handleAgent)
	mux.HandleFunc("/agent/notify-policy", r.handleAgentNotifyPolicy)
	mux.HandleFunc("/agent/events", r.handleAgentEvent)
	mux.HandleFunc("/dial", r.handleDial)
	mux.HandleFunc("/session/", r.handleSession)
	mux.HandleFunc("/peers", r.handlePeers)
	// HTTP transport (proxy-agnostic; no WebSocket upgrade required).
	mux.HandleFunc("/h/poll", r.handleHPoll)
	mux.HandleFunc("/h/dial", r.handleHDial)
	mux.HandleFunc("/h/peers", r.handleHPeers)
	mux.HandleFunc("/h/up", r.handleHUp)
	mux.HandleFunc("/h/down", r.handleHDown)
	mux.HandleFunc("/h/close", r.handleHClose)
	mux.HandleFunc("/h/deregister", r.handleHDeregister)
	mux.HandleFunc("/enroll/exchange", r.handleEnrollExchange)
	r.registerDocs(mux)
	r.registerAdmin(mux)
	r.registerUser(mux)
	r.registerDist(mux)
	if r.mcpHandler != nil {
		// AI hosts register https://<relay>/wanctl-mcp as their MCP server URL.
		// (We can't use /mcp directly: thunderbox's edge nginx reserves any
		// path starting with /mcp for its own gateway, returning 401 Bearer
		// Token required before our backend sees the request.)
		mux.Handle("/wanctl-mcp", r.mcpHandler)
		mux.Handle("/wanctl-mcp/", r.mcpHandler)
	}
	return limitBodies(mux)
}

// limitBodies puts a size cap on every request body before a handler decodes
// it. The tunnel routes are exempt: WebSocket upgrades carry no body, and
// /h/up applies its own per-write cap (limits.RelayHTTPUploadBytes). A JSON
// decoder that hits the cap returns *http.MaxBytesError and the server closes
// the connection, so a handler that ignores the decode error still cannot be
// made to buffer more than the cap.
func limitBodies(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Body != nil && req.Body != http.NoBody {
			if n := bodyCapFor(req.URL.Path); n > 0 {
				req.Body = http.MaxBytesReader(w, req.Body, n)
			}
		}
		next.ServeHTTP(w, req)
	})
}

// bodyCapFor returns the body cap for a route, or 0 for routes that bound
// themselves.
func bodyCapFor(path string) int64 {
	switch {
	case path == "/h/up":
		return 0
	case strings.HasPrefix(path, "/wanctl-mcp"):
		return limits.RelayMCPBodyBytes
	case strings.HasPrefix(path, "/docs/"), strings.HasPrefix(path, "/admin/docs/"):
		return limits.RelayDocsBodyBytes
	default:
		return limits.RelayControlBodyBytes
	}
}

// SetMCPHandler installs the HTTP/Streamable MCP handler the relay will expose
// at GET/POST /mcp. Pass nil (or never call) to disable the endpoint.
func (r *Relay) SetMCPHandler(h http.Handler) { r.mcpHandler = h }

// SetAdmin installs the admin store backing the /admin/* endpoints.
func (r *Relay) SetAdmin(a AdminStore) {
	r.admin = a
	if store, ok := a.(DeviceAliasStore); ok {
		r.aliases = store
	} else {
		r.aliases = nil
	}
	if store, ok := a.(NotifyStore); ok {
		r.notifyStore = store
	} else {
		r.notifyStore = nil
	}
}

// SetNotifySender replaces outbound delivery, primarily for deterministic tests.
func (r *Relay) SetNotifySender(sender webhookSender) { r.notifySend = sender }

// SetLogBuffer installs the process-local service log buffer.
func (r *Relay) SetLogBuffer(logs *serverlog.Buffer) { r.logs = logs }

func (r *Relay) auth(w http.ResponseWriter, req *http.Request) (ns string, ok bool) {
	token, legacy, ok := admission.Token(req)
	if !ok {
		return "", false
	}
	if legacy {
		admission.MarkLegacy(w)
	}
	return r.ts.Resolve(token)
}

// SetACL installs an ACL checker for cross-namespace dials.
func (r *Relay) SetACL(c ACLChecker) { r.acl = c }

// SetAuditor installs a metadata audit sink.
func (r *Relay) SetAuditor(a Auditor) { r.audit = a }

// SetPortalNS marks a namespace as the privileged portal: tokens resolving to it
// may dial any device (the device still enforces E2E trust + policy).
func (r *Relay) SetPortalNS(ns string) { r.portalNS = ns }

// dialAllowed splits target into namespace/device and checks access for caller.
func (r *Relay) dialAllowed(callerNS, target string) (targetKey string, auth sessionauth.Open, ok bool) {
	if !strings.Contains(target, "/") {
		target = callerNS + "/" + target
	}
	i := strings.Index(target, "/")
	targetNS, device := target[:i], target[i+1:]
	if r.aliases != nil {
		if resolved, found := r.aliases.ResolveDeviceTarget(targetNS, device); found {
			device = resolved
			target = targetNS + "/" + device
		}
	}
	auth = sessionauth.Open{CallerNamespace: callerNS, OwnerNamespace: targetNS, Device: device}
	if r.portalNS != "" && callerNS == r.portalNS {
		auth.Capabilities = sessionauth.FullCapabilities
		return target, auth, true
	}
	if targetNS == callerNS {
		auth.Capabilities = sessionauth.FullCapabilities
		return target, auth, true
	}
	if r.acl != nil {
		if perms, found := r.acl.ACLPerms(callerNS, targetNS, device); found {
			caps, err := sessionauth.ParseGrant(perms)
			if err == nil {
				auth.Capabilities = caps
				return target, auth, true
			}
		}
	}
	return target, auth, false
}

func newID() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (r *Relay) handleAgent(w http.ResponseWriter, req *http.Request) {
	ns, ok := r.auth(w, req)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	c, err := websocket.Accept(w, req, nil)
	if err != nil {
		return
	}
	limits.ClearHijackedDeadline(req.Context())
	nc := wsconn.FromAccepted(req.Context(), c)
	dec := json.NewDecoder(nc)
	var reg struct {
		Op, Device, Fingerprint, Inst string
	}
	if err := dec.Decode(&reg); err != nil || reg.Op != "register" || reg.Device == "" {
		c.Close(websocket.StatusPolicyViolation, "expected register")
		return
	}
	key := ns + "/" + reg.Device
	wasLive := r.deviceLive(ns, reg.Device)
	ac := &agentConn{device: reg.Device, ns: ns, inst: reg.Inst, ctrl: nc}
	r.mu.Lock()
	r.agents[key] = ac
	r.mu.Unlock()
	created := r.recordDeviceRegistration(ns, reg.Device, reg.Fingerprint)
	if !wasLive {
		r.emitDeviceEvent(ns, reg.Device, onlineEvent(reg.Device))
	}
	if created {
		r.emitAccountEvent(ns, enrollEvent(reg.Device))
	}
	defer func() {
		removed := false
		r.mu.Lock()
		if r.agents[key] == ac {
			delete(r.agents, key)
			removed = true
		}
		r.mu.Unlock()
		if removed && !r.deviceLive(ns, reg.Device) {
			r.emitDeviceEvent(ns, reg.Device, offlineEvent(reg.Device))
		}
		c.Close(websocket.StatusNormalClosure, "")
	}()
	// Keep the control connection alive; drain any further messages (e.g. pings).
	for {
		var ignore json.RawMessage
		if err := dec.Decode(&ignore); err != nil {
			return
		}
	}
}

func (r *Relay) handleDial(w http.ResponseWriter, req *http.Request) {
	ns, ok := r.auth(w, req)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	targetKey, auth, ok := r.dialAllowed(ns, req.URL.Query().Get("target"))
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	r.mu.Lock()
	ac := r.agents[targetKey]
	r.mu.Unlock()
	if ac == nil {
		r.handleWSDialToHTTP(w, req, targetKey, auth)
		return
	}
	if r.audit != nil {
		r.audit.Audit(auth.OwnerNamespace, auth.Device, "dial")
	}
	sid := newID()
	ps := &pendingSession{agentSide: make(chan io.ReadWriteCloser, 1), done: make(chan struct{})}
	r.mu.Lock()
	r.pending[sid] = ps
	r.mu.Unlock()
	defer func() {
		close(ps.done)
		r.mu.Lock()
		delete(r.pending, sid)
		r.mu.Unlock()
	}()

	auth.Op = "open"
	auth.Session = sid
	auth.URL = "/session/" + sid
	if err := ac.send(auth); err != nil {
		http.Error(w, "agent unreachable", http.StatusBadGateway)
		return
	}
	c, err := websocket.Accept(w, req, nil)
	if err != nil {
		return
	}
	limits.ClearHijackedDeadline(req.Context())
	clientNC := wsconn.FromAccepted(req.Context(), c)

	select {
	case agentNC := <-ps.agentSide:
		pipe(clientNC, agentNC)
	case <-time.After(agentSessionOpenTimeout):
		c.Close(websocket.StatusBadGateway, "agent did not open session")
	}
}

func (r *Relay) handleSession(w http.ResponseWriter, req *http.Request) {
	if _, ok := r.auth(w, req); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sid := strings.TrimPrefix(req.URL.Path, "/session/")
	r.mu.Lock()
	ps := r.pending[sid]
	r.mu.Unlock()
	if ps == nil {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	c, err := websocket.Accept(w, req, nil)
	if err != nil {
		return
	}
	limits.ClearHijackedDeadline(req.Context())
	agentNC := wsconn.FromAccepted(req.Context(), c)
	defer agentNC.Close()
	select {
	case ps.agentSide <- agentNC:
		<-ps.done
	case <-ps.done:
		c.Close(websocket.StatusGoingAway, "client went away")
	}
}

func (r *Relay) handlePeers(w http.ResponseWriter, req *http.Request) {
	ns, ok := r.auth(w, req)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	devices, aliases := r.livePeers(ns)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"namespace": ns, "devices": devices, "aliases": aliases})
}

// pipe copies bytes both directions until either side closes, then tears down.
func pipe(a, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }()
	go func() { io.Copy(b, a); done <- struct{}{} }()
	<-done
	a.Close()
	b.Close()
}
