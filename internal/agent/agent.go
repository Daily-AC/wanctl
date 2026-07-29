// Package agent is the controlled node. It dials the relay outbound, registers a
// device name, and for each session the relay opens it completes the server-side
// mutual-TLS handshake, applies TOFU authorization, and serves exec/file
// requests using the forked server handlers.
package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wanctl/internal/admission"
	"wanctl/internal/config"
	"wanctl/internal/console"
	"wanctl/internal/eventlog"
	"wanctl/internal/httpconn"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
	"wanctl/internal/server"
	"wanctl/internal/sessionauth"
	"wanctl/internal/transport"
	"wanctl/internal/wsconn"

	"golang.org/x/term"
)

// Options configures an agent run.
type Options struct {
	RelayURL  string // ws(s)://host[:port], no path
	Token     string
	Name      string
	Shell     string
	AutoYes   bool
	Transport string      // "ws" (default) or "http" (proxy-agnostic)
	Mode      policy.Mode // "normal" (default) or "bypass"
	PortalFP  string      // deprecated single portal admin fingerprint
	PortalFPs []string    // pre-trusted portal admin fingerprints, enrolled locally
	LanRelay  string      // intranet fast-path relay (ws://...); "" disables the second uplink
}

// Agent is a running controlled node.
type Agent struct {
	id           *transport.Identity
	known        *transport.Store
	portalAdmins *config.PortalAdmins
	opts         Options
	engine       *policy.Engine
	console      *console.Service
	log          *eventlog.Logger
	inst         string
	apprMu       sync.Mutex
	appr         policy.Approver

	sessMu   sync.Mutex
	sessions map[string]*server.ShellSession
	jobs     *jobStore
	stdin    *bufio.Reader

	lanMu        sync.Mutex
	lanEnabled   bool
	lanConnected bool
	lanKick      chan struct{} // wakes the LAN loop when the switch flips
}

// New constructs an Agent with loaded identity, controller allow-list, and
// policy engine. The approver is the queue-backed console service; a local CLI
// terminal and/or a connected portal can both feed decisions into the same
// queue. Headless + no portal connected means the 60 s timeout deny fires.
func New(opts Options) (*Agent, error) {
	id, err := transport.LoadOrCreateIdentity()
	if err != nil {
		return nil, err
	}
	known, err := transport.OpenStore("known_clients.json")
	if err != nil {
		return nil, err
	}
	portalAdmins, err := config.OpenPortalAdmins()
	if err != nil {
		return nil, err
	}
	portalFPs := append([]string(nil), opts.PortalFPs...)
	if opts.PortalFP != "" {
		portalFPs = append(portalFPs, opts.PortalFP)
	}
	// Before portal_admins.json existed, installer-enrolled roots were stored as
	// ordinary known clients named "portal". Promote that explicit legacy marker
	// on first startup so upgrades retain rotation and last-root protection.
	for _, peer := range known.List() {
		if peer.Name == "portal" {
			portalFPs = append(portalFPs, peer.Fingerprint)
		}
	}
	if err := portalAdmins.Add(portalFPs...); err != nil {
		return nil, fmt.Errorf("seed portal admins: %w", err)
	}
	for _, fp := range portalAdmins.List() {
		if !known.Has(fp) {
			if err := known.Add(fp, "portal"); err != nil {
				return nil, fmt.Errorf("trust portal admin %s: %w", fp, err)
			}
		}
	}
	if opts.Shell == "" {
		opts.Shell = server.DefaultShell()
	}
	if opts.Mode == "" {
		opts.Mode = policy.ModeNormal
	}
	if opts.Name == "" {
		h, _ := os.Hostname()
		if h == "" {
			h = "wanctl-agent"
		}
		opts.Name = h
	}
	inst, err := newInstanceID()
	if err != nil {
		return nil, err
	}
	engine, err := policy.Open("rules.json", opts.Mode)
	if err != nil {
		return nil, err
	}
	logger, err := eventlog.Open("events.jsonl")
	if err != nil {
		return nil, err
	}
	a := &Agent{
		id: id, known: known, portalAdmins: portalAdmins, opts: opts, engine: engine, log: logger,
		inst:     inst,
		sessions: map[string]*server.ShellSession{}, jobs: newJobStore(), stdin: bufio.NewReader(os.Stdin),
	}
	a.console = console.New(engine, logger, console.Info{
		Device: opts.Name, Fingerprint: id.Fingerprint, Relay: opts.RelayURL,
	})
	a.console.SetTrustedSource(a.trustedControllers)
	a.lanEnabled = config.LanUplinkEnabled()
	a.lanKick = make(chan struct{}, 1)
	if opts.LanRelay != "" {
		a.console.SetLanSource(a.lanInfo)
	}
	if opts.Mode == policy.ModeBypass {
		a.appr = policy.AllowApprover{}
	} else {
		// Queue-backed approver: the remote portal and/or the local CLI terminal
		// feed decisions into the same queue. Headless + no portal -> timeout deny.
		a.appr = a.console
	}
	return a, nil
}

func newInstanceID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate agent instance id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// setApprover overrides the approver (used by tests).
func (a *Agent) setApprover(ap policy.Approver) {
	a.apprMu.Lock()
	a.appr = ap
	a.apprMu.Unlock()
}

type dataCapability string

const capabilityReadEventLog dataCapability = "read-event-log"

// gateDataCapability keeps data-session capabilities distinct from exec and
// file requests. A later identity/capability layer can deny here before the
// existing interactive policy gate without changing the wire handlers.
func (a *Agent) gateDataCapability(cap dataCapability, peerFP string) (bool, string) {
	switch cap {
	case capabilityReadEventLog:
		return a.gate(policy.Request{Kind: policy.KindLogs, Peer: peerFP})
	default:
		return false, "unsupported-capability"
	}
}

// gate authorizes a request: bypass/pre-approved pass; otherwise ask the
// approver and optionally remember a rule. Returns whether the op may proceed
// and a short decision string for the audit log.
func (a *Agent) gate(req policy.Request) (bool, string) {
	if a.engine.Mode() == policy.ModeBypass {
		return true, "bypass"
	}
	if a.engine.Allowed(req) {
		return true, "pre-approved"
	}
	a.apprMu.Lock()
	appr := a.appr
	a.apprMu.Unlock()
	d := appr.Ask(req)
	if !d.Allow {
		return false, "denied"
	}
	if d.Remember {
		a.engine.Add(policy.RuleFor(req, d.Scope))
		return true, "remembered:" + string(d.Scope)
	}
	return true, "approved"
}

// gateFile returns the policy root that must constrain the actual filesystem
// open. A one-shot approval is restricted to the requested file's parent;
// global and bypass decisions use an empty root, meaning the filesystem volume.
func (a *Agent) gateFile(req policy.Request) (bool, string, string) {
	if a.engine.Mode() == policy.ModeBypass {
		return true, "bypass", ""
	}
	if root, ok := a.engine.AllowedFileRoot(req); ok {
		return true, "pre-approved", root
	}
	a.apprMu.Lock()
	appr := a.appr
	a.apprMu.Unlock()
	d := appr.Ask(req)
	if !d.Allow {
		return false, "denied", ""
	}
	if d.Remember {
		rule := policy.RuleFor(req, d.Scope)
		a.engine.Add(rule)
		return true, "remembered:" + string(d.Scope), rule.Pattern
	}
	return true, "approved", filepath.Dir(req.Path)
}

// Run connects the control channel and serves sessions until ctx is cancelled.
// If a LAN relay is configured, a second uplink to it runs alongside the
// primary one; sessions from either relay are served identically (same E2E
// trust, same policy gate).
func (a *Agent) Run(ctx context.Context) error {
	if a.opts.LanRelay != "" {
		go a.runLan(ctx)
	}
	if a.opts.Transport == "http" {
		return a.runHTTP(ctx)
	}
	ctrlURL := strings.TrimRight(a.opts.RelayURL, "/") + "/agent"
	nc, resp, err := wsconn.Dial(ctx, ctrlURL, admission.Header(a.opts.Token))
	if err != nil {
		if resp != nil && resp.StatusCode == 401 {
			return fmt.Errorf("relay rejected token (401)")
		}
		return fmt.Errorf("connect relay: %w", err)
	}
	defer nc.Close()
	// Without this the Decode below parks forever on an idle control channel and
	// cancellation (SIGTERM, `wanctl stop`) is never observed.
	defer wsconn.CloseOnCancel(ctx, nc)()
	enc := json.NewEncoder(nc)
	if err := enc.Encode(map[string]string{"op": "register", "device": a.opts.Name, "fingerprint": a.id.Fingerprint}); err != nil {
		return err
	}
	fmt.Printf("wanctl agent %q online via %s\n  fingerprint: %s\n", a.opts.Name, a.opts.RelayURL, a.id.Fingerprint)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		go a.runConsolePrompt(ctx)
	}

	dec := json.NewDecoder(bufio.NewReader(nc))
	for {
		var msg sessionauth.Open
		if err := dec.Decode(&msg); err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return fmt.Errorf("control channel closed: %w", err)
			}
		}
		if msg.Op == "open" && msg.ValidFor(a.opts.Name) {
			go a.serveSession(ctx, msg)
		}
	}
}

func (a *Agent) serveSession(ctx context.Context, open sessionauth.Open) {
	a.serveSessionWS(ctx, a.opts.RelayURL, open, nil)
}

// serveSessionWS opens the per-session WebSocket on the given relay and serves
// it. hc overrides the handshake HTTP client (NoProxyClient for the intranet
// relay).
func (a *Agent) serveSessionWS(ctx context.Context, relayURL string, open sessionauth.Open, hc *http.Client) {
	url := strings.TrimRight(relayURL, "/") + open.URL
	nc, _, err := wsconn.DialWith(ctx, url, admission.Header(a.opts.Token), hc)
	if err != nil {
		return
	}
	a.handleSession(ctx, nc, open)
}

// handleSession completes the server-side handshake and serves requests on an
// already-established transport conn (WebSocket or HTTP).
func (a *Agent) handleSession(ctx context.Context, nc net.Conn, auth sessionauth.Open) {
	conn, fp, err := transport.ServerHandshake(ctx, nc, a.id)
	if err != nil {
		return
	}
	defer conn.Close()

	hello, err := protocol.ReadMessage(conn)
	if err != nil {
		return
	}
	if hello.Kind != protocol.KindHello && hello.Kind != protocol.KindConsoleHello {
		return
	}
	if !auth.ValidFor(a.opts.Name) {
		protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindReject, Reason: "invalid relay session capabilities"})
		return
	}
	// Pairing grants a controller permission to submit device operations; it
	// must not grant the control-plane capability to approve those operations,
	// change rules, or enable bypass mode. Only the enrolled portal identity is
	// a console administrator, and the relay session must independently carry
	// the console capability. An empty administrator set therefore fails closed.
	if hello.Kind == protocol.KindConsoleHello {
		if !auth.Capabilities.Has(sessionauth.Console) {
			protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindReject, Reason: "session capability denied: console"})
			return
		}
		if a.portalAdmins == nil || !a.portalAdmins.Contains(fp) {
			protocol.WriteMessage(conn, protocol.Message{
				Kind:   protocol.KindReject,
				Reason: "controller is not authorized as this device's console administrator",
			})
			return
		}
	}
	// Authorize (TOFU / pre-trusted portal key) and reply OK for BOTH exec and
	// console sessions BEFORE serving — the controller/portal blocks on this OK,
	// and a console session must be gated by the same trust check as an exec one.
	if !a.authorize(fp, hello.Name, hello.Label) {
		protocol.WriteMessage(conn, protocol.Message{
			Kind:       protocol.KindReject,
			Reason:     "device has not paired this controller — ask the user to approve",
			PairingURL: a.pairingURL(fp, hello.Name, hello.Label),
		})
		return
	}
	protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindOK, Name: a.opts.Name})
	a.log.Append(eventlog.Event{Type: "connect", PeerFP: fp, PeerName: hello.Name})
	if hello.Kind == protocol.KindConsoleHello {
		a.serveConsole(ctx, conn)
		return
	}
	a.serve(conn, fp, hello.Name, auth.Capabilities)
}

func (a *Agent) authorize(fp, name, label string) bool {
	if a.known.Has(fp) {
		a.known.Touch(fp)
		return true
	}
	if a.opts.AutoYes {
		a.known.AddLabeled(fp, name, label)
		fmt.Printf("[auto-trust] new controller %q paired: %s\n", name, fp)
		return true
	}
	// Surface the pairing request to a connected front-end (the portal web
	// console) and block for a human's trust decision. A headless agent with no
	// portal connected denies (pre-trust with --portal-fps or --yes instead).
	if a.console.AskPair(fp, name, label) {
		a.known.AddLabeled(fp, name, label)
		fmt.Printf("[paired] controller %q trusted via console: %s\n", name, fp)
		return true
	}
	return false
}

// pairingURL builds the portal URL a user clicks to trust this controller. The
// AI surfaces it verbatim in its reply ("ask the user to click this link"); the
// SPA's #pair route reads device/fp/label and shows a confirmation card.
func (a *Agent) pairingURL(fp, name, label string) string {
	q := url.Values{}
	q.Set("device", a.opts.Name)
	q.Set("fp", fp)
	if name != "" {
		q.Set("name", name)
	}
	if label != "" {
		q.Set("label", label)
	}
	return config.DefaultPortal + "/#pair?" + q.Encode()
}

// trustedControllers lists currently trusted controllers for the console revoke UI.
func (a *Agent) trustedControllers() []console.TrustedController {
	out := []console.TrustedController{}
	for _, p := range a.known.List() {
		ls := ""
		if !p.LastSeen.IsZero() {
			ls = p.LastSeen.Format(time.RFC3339)
		}
		out = append(out, console.TrustedController{FP: p.Fingerprint, Name: p.Name, Label: p.Label, LastSeen: ls})
	}
	return out
}

func (a *Agent) serve(conn *tls.Conn, fp, peerName string, caps sessionauth.Capabilities) {
	for {
		m, err := protocol.ReadMessage(conn)
		if err != nil {
			return
		}
		if required := requiredCapability(m.Kind); required != 0 && !caps.Has(required) {
			protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindReject, Reason: "session capability denied: " + required.String()})
			continue
		}
		switch m.Kind {
		case protocol.KindExec:
			a.doExec(conn, fp, peerName, m)
		case protocol.KindExecAsync:
			a.doExecAsync(conn, fp, peerName, m)
		case protocol.KindExecPoll:
			a.doExecPoll(conn, m)
		case protocol.KindLogs:
			ok, decision := a.gateDataCapability(capabilityReadEventLog, fp)
			a.log.Append(eventlog.Event{
				Type: "logs", PeerFP: fp, PeerName: peerName,
				Detail: "read event log", Decision: decision,
			})
			if !ok {
				protocol.WriteMessage(conn, protocol.Message{
					Kind: protocol.KindReject, Reason: "event log access denied by device policy",
				})
				continue
			}
			a.doLogs(conn, m)
		case protocol.KindFilePut:
			ok, decision, root := a.gateFile(policy.Request{Kind: policy.KindWrite, Path: m.Path, Peer: fp})
			a.log.Append(eventlog.Event{Type: "file", PeerFP: fp, PeerName: peerName, Detail: "PUT " + m.Path, Decision: decision})
			if !ok {
				protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindReject, Reason: "write denied by device policy: " + m.Path})
				continue
			}
			server.HandleFilePut(conn, m, root)
		case protocol.KindFileGet:
			ok, decision, root := a.gateFile(policy.Request{Kind: policy.KindRead, Path: m.Path, Peer: fp})
			a.log.Append(eventlog.Event{Type: "file", PeerFP: fp, PeerName: peerName, Detail: "GET " + m.Path, Decision: decision})
			if !ok {
				protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindReject, Reason: "read denied by device policy: " + m.Path})
				continue
			}
			server.HandleFileGet(conn, m, root)
		default:
			protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: "unknown request: " + m.Kind})
			return
		}
	}
}

func requiredCapability(kind string) sessionauth.Capabilities {
	switch kind {
	case protocol.KindExec, protocol.KindExecAsync, protocol.KindExecPoll:
		return sessionauth.Exec
	case protocol.KindFileGet:
		return sessionauth.Read
	case protocol.KindFilePut:
		return sessionauth.Write
	case protocol.KindLogs:
		return sessionauth.Logs
	default:
		return 0
	}
}

func (a *Agent) doExec(conn *tls.Conn, fp, peerName string, m protocol.Message) {
	ok, decision := a.gate(policy.Request{Kind: policy.KindExec, Cmd: m.Command, Cwd: m.Cwd, Peer: fp})
	if !ok {
		a.log.Append(eventlog.Event{Type: "exec", PeerFP: fp, PeerName: peerName, Detail: m.Command, Cwd: m.Cwd, Decision: decision})
		protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindReject, Reason: "command denied by device policy: " + m.Command})
		return
	}
	out := server.FrameWriter(conn, protocol.FrameStdout)
	var code int
	var err error
	if m.OneShot {
		code, err = server.RunOneShot(a.opts.Shell, m.Command, m.Cwd, out)
	} else {
		sess, serr := a.session(fp)
		if serr != nil {
			protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: serr.Error()})
			return
		}
		code, err = sess.ExecInDir(m.Command, m.Cwd, out)
	}
	if err != nil {
		a.log.Append(eventlog.Event{Type: "exec", PeerFP: fp, PeerName: peerName, Detail: m.Command, Cwd: m.Cwd, Decision: decision})
		protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: err.Error()})
		return
	}
	a.log.Append(eventlog.Event{Type: "exec", PeerFP: fp, PeerName: peerName, Detail: m.Command, Cwd: m.Cwd, Decision: decision, Exit: &code})
	protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindExit, Code: code})
}

// doExecAsync starts a command as a background job and returns its id at once,
// without waiting for it to finish. The job keeps running on the device after
// this connection closes; the controller fetches output and exit code later via
// doExecPoll. This decouples long commands from any per-request timeout (#2) and
// makes a once-orphaned process queryable (#16). Async jobs always run in a
// fresh shell (no shared persistent-session state).
func (a *Agent) doExecAsync(conn *tls.Conn, fp, peerName string, m protocol.Message) {
	ok, decision := a.gate(policy.Request{Kind: policy.KindExec, Cmd: m.Command, Cwd: m.Cwd, Peer: fp})
	if !ok {
		a.log.Append(eventlog.Event{Type: "exec", PeerFP: fp, PeerName: peerName, Detail: "[async] " + m.Command, Cwd: m.Cwd, Decision: decision})
		protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindReject, Reason: "command denied by device policy: " + m.Command})
		return
	}
	id, err := a.jobs.start(a.opts.Shell, m.Command, m.Cwd)
	if err != nil {
		protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: err.Error()})
		return
	}
	a.log.Append(eventlog.Event{Type: "exec", PeerFP: fp, PeerName: peerName, Detail: "[async " + id + "] " + m.Command, Cwd: m.Cwd, Decision: decision})
	protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindOK, JobID: id})
}

// doExecPoll streams a background job's output past m.Offset, then reports the
// new total length and whether it is still running. The job id (a random secret
// returned at start) is the capability, so no extra policy gate is applied — the
// command itself was gated when it started.
func (a *Agent) doExecPoll(conn *tls.Conn, m protocol.Message) {
	j := a.jobs.get(m.JobID)
	if j == nil {
		protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: "unknown or expired job: " + m.JobID})
		return
	}
	newOut, total, done, code := j.snapshot(m.Offset)
	if len(newOut) > 0 {
		protocol.WriteFrame(conn, protocol.FrameStdout, newOut)
	}
	protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindExit, Offset: total, Running: !done, Code: code})
}

// doLogs streams matching local events back to the controller as JSON lines.
func (a *Agent) doLogs(conn *tls.Conn, m protocol.Message) {
	f := eventlog.Filter{Type: m.LogType, Grep: m.Grep, Limit: int(m.Limit)}
	if m.Since != "" {
		if ts, err := time.Parse(time.RFC3339, m.Since); err == nil {
			f.Since = ts
		}
	}
	events, err := a.log.Read(f)
	if err != nil {
		protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: err.Error()})
		return
	}
	out := server.FrameWriter(conn, protocol.FrameStdout)
	for _, e := range events {
		b, _ := json.Marshal(e)
		out.Write(append(b, '\n'))
	}
	protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindExit, Code: 0})
}

// Mode reports the agent's effective policy mode (which may be a mode persisted
// from a previous run, not just the one passed at construction).
func (a *Agent) Mode() policy.Mode { return a.engine.Mode() }

// httpBase converts the relay URL to an HTTP(S) origin for the HTTP transport.
func httpBase(relayURL string) string {
	b := strings.TrimRight(relayURL, "/")
	if strings.HasPrefix(b, "wss:") {
		return "https:" + b[4:]
	}
	if strings.HasPrefix(b, "ws:") {
		return "http:" + b[3:]
	}
	return b
}

// runHTTP drives the proxy-agnostic HTTP transport: long-poll /h/poll for
// sessions to open, and serve each over an httpconn.
// deregisterHTTP best-effort tells the relay we're going offline now, so the
// device flips to offline immediately instead of waiting out the registry TTL.
// Called on clean shutdown; uses a fresh short-timeout client since the run ctx
// is already cancelled.
func (a *Agent) deregisterHTTP(base string) {
	qv := url.Values{"device": {a.opts.Name}}
	if a.inst != "" {
		qv.Set("inst", a.inst)
	}
	q := qv.Encode()
	hc := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequest("POST", base+"/h/deregister?"+q, nil)
	if err != nil {
		return
	}
	admission.SetBearer(req, a.opts.Token)
	if resp, err := hc.Do(req); err == nil {
		resp.Body.Close()
	}
}

func (a *Agent) runHTTP(ctx context.Context) error {
	base := httpBase(a.opts.RelayURL)
	fmt.Printf("wanctl agent %q online via %s (http transport)\n  fingerprint: %s\n", a.opts.Name, base, a.id.Fingerprint)
	hc := &http.Client{Timeout: 35 * time.Second}
	q := url.Values{"device": {a.opts.Name}, "fp": {a.id.Fingerprint}, "inst": {a.inst}}.Encode()
	pollURL := base + "/h/poll?" + q
	for {
		if ctx.Err() != nil {
			a.deregisterHTTP(base)
			return nil
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", pollURL, nil)
		admission.SetBearer(req, a.opts.Token)
		resp, err := hc.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				a.deregisterHTTP(base)
				return nil
			}
			if strings.Contains(err.Error(), "401") {
				return fmt.Errorf("relay rejected token")
			}
			time.Sleep(2 * time.Second) // backoff then re-poll
			continue
		}
		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			return fmt.Errorf("relay rejected token (401)")
		}
		if resp.StatusCode == http.StatusConflict {
			resp.Body.Close()
			return fmt.Errorf("another wanctl agent instance registered this device name; this instance is standing down")
		}
		var msg sessionauth.Open
		json.NewDecoder(resp.Body).Decode(&msg)
		resp.Body.Close()
		if msg.ValidFor(a.opts.Name) {
			go a.serveSessionHTTP(ctx, base, msg)
		}
	}
}

func (a *Agent) serveSessionHTTP(ctx context.Context, base string, open sessionauth.Open) {
	nc, err := httpconn.Dial(ctx, base, open.Session, "agent", a.opts.Token)
	if err != nil {
		return
	}
	a.handleSession(ctx, nc, open)
}

// lanInfo snapshots the LAN-uplink state for the console/portal.
func (a *Agent) lanInfo() *console.LanInfo {
	a.lanMu.Lock()
	defer a.lanMu.Unlock()
	return &console.LanInfo{Relay: a.opts.LanRelay, Enabled: a.lanEnabled, Connected: a.lanConnected}
}

func (a *Agent) setLanConnected(v bool) {
	a.lanMu.Lock()
	changed := a.lanConnected != v
	a.lanConnected = v
	a.lanMu.Unlock()
	if changed {
		a.console.Notify() // push fresh state to any connected portal
	}
}

// SetLanEnabled flips the device-side LAN-uplink switch (portal RPC / CLI),
// persists it, and kicks the LAN loop so it reacts immediately.
func (a *Agent) SetLanEnabled(on bool) {
	a.lanMu.Lock()
	a.lanEnabled = on
	a.lanMu.Unlock()
	_ = config.SaveLanUplink(on)
	select {
	case a.lanKick <- struct{}{}:
	default:
	}
	a.console.Notify()
}

func (a *Agent) lanIsEnabled() bool {
	a.lanMu.Lock()
	defer a.lanMu.Unlock()
	return a.lanEnabled
}

// runLan maintains the second uplink to the intranet relay: register, serve
// sessions, reconnect with quiet backoff. Unreachable relay (device outside
// the company network) just means periodic cheap dial failures. The uplink
// only ever uses the WS transport — the intranet relay has no proxy in front.
func (a *Agent) runLan(ctx context.Context) {
	const backoff = 30 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if !a.lanIsEnabled() {
			a.setLanConnected(false)
			select {
			case <-ctx.Done():
				return
			case <-a.lanKick:
			}
			continue
		}
		err := a.runLanOnce(ctx)
		a.setLanConnected(false)
		if ctx.Err() != nil {
			return
		}
		_ = err // quiet: expected whenever the device is outside the intranet
		select {
		case <-ctx.Done():
			return
		case <-a.lanKick:
		case <-time.After(backoff):
		}
	}
}

// runLanOnce holds one registered control channel on the intranet relay until
// it drops or the switch turns off.
func (a *Agent) runLanOnce(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	ctrlURL := strings.TrimRight(a.opts.LanRelay, "/") + "/agent"
	nc, _, err := wsconn.DialWith(dialCtx, ctrlURL, admission.Header(a.opts.Token), wsconn.NoProxyClient)
	cancel()
	if err != nil {
		return err
	}
	defer nc.Close()
	enc := json.NewEncoder(nc)
	if err := enc.Encode(map[string]string{"op": "register", "device": a.opts.Name, "fingerprint": a.id.Fingerprint}); err != nil {
		return err
	}
	fmt.Printf("wanctl agent %q: LAN uplink online via %s\n", a.opts.Name, a.opts.LanRelay)
	a.setLanConnected(true)

	// Tear the conn down when the switch flips off so the read below unblocks.
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		for {
			select {
			case <-watchDone:
				return
			case <-ctx.Done():
				nc.Close()
				return
			case <-a.lanKick:
				if !a.lanIsEnabled() {
					nc.Close()
					return
				}
			}
		}
	}()

	dec := json.NewDecoder(bufio.NewReader(nc))
	for {
		var msg sessionauth.Open
		if err := dec.Decode(&msg); err != nil {
			return err
		}
		if msg.Op == "open" && msg.ValidFor(a.opts.Name) {
			go a.serveSessionWS(ctx, a.opts.LanRelay, msg, wsconn.NoProxyClient)
		}
	}
}

func (a *Agent) session(fp string) (*server.ShellSession, error) {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	if sess, ok := a.sessions[fp]; ok && !sess.Closed() {
		return sess, nil
	}
	sess, err := server.NewShellSession(a.opts.Shell)
	if err != nil {
		return nil, err
	}
	a.sessions[fp] = sess
	return sess, nil
}

// handleConsoleRPC dispatches a single console RPC message and returns the response.
// It is a pure function: no goroutines, no writes to conn.
func (a *Agent) handleConsoleRPC(msg protocol.Message) protocol.Message {
	switch msg.Kind {
	case protocol.KindConsoleState:
		snap := a.console.State()
		data, _ := json.Marshal(snap)
		return protocol.Message{Kind: protocol.KindConsoleState, Data: json.RawMessage(data)}

	case protocol.KindDecide:
		ok := a.console.Decide(msg.ApprovalID, msg.Verdict)
		verdict := "ok"
		if !ok {
			verdict = "not-found"
		}
		return protocol.Message{Kind: protocol.KindDecide, Verdict: verdict}

	case protocol.KindRuleAdd:
		err := a.console.AddRule(policy.Rule{
			Kind:    policy.Kind(msg.RuleKind),
			Pattern: msg.Pattern,
			Dir:     msg.Dir,
			Scope:   policy.Scope(msg.Scope),
		})
		resp := protocol.Message{Kind: protocol.KindRuleAdd}
		if err != nil {
			errJSON, _ := json.Marshal(err.Error())
			resp.Data = json.RawMessage(errJSON)
		}
		return resp

	case protocol.KindRuleRm:
		err := a.console.RemoveRule(msg.Index)
		resp := protocol.Message{Kind: protocol.KindRuleRm}
		if err != nil {
			errJSON, _ := json.Marshal(err.Error())
			resp.Data = json.RawMessage(errJSON)
		}
		return resp

	case protocol.KindModeSet:
		a.console.SetMode(policy.Mode(msg.ConsoleMode))
		return protocol.Message{Kind: protocol.KindModeSet}

	case protocol.KindPairDecide:
		ok := a.console.DecidePair(msg.FP, msg.Verdict == "y")
		resp := protocol.Message{Kind: protocol.KindPairDecide}
		if !ok {
			errJSON, _ := json.Marshal("no such pending pairing")
			resp.Data = json.RawMessage(errJSON)
		}
		return resp

	case protocol.KindTrustRevoke:
		resp := protocol.Message{Kind: protocol.KindTrustRevoke}
		removedPortalAdmin := false
		if a.portalAdmins != nil && a.portalAdmins.Contains(msg.FP) {
			if err := a.portalAdmins.Remove(msg.FP); err != nil {
				errJSON, _ := json.Marshal(err.Error())
				resp.Data = json.RawMessage(errJSON)
				return resp
			}
			removedPortalAdmin = true
		}
		if err := a.known.Remove(msg.FP); err != nil {
			if removedPortalAdmin {
				_ = a.portalAdmins.Add(msg.FP)
			}
			errJSON, _ := json.Marshal(err.Error())
			resp.Data = json.RawMessage(errJSON)
		} else {
			a.console.Notify()
		}
		return resp

	case protocol.KindLanSet:
		resp := protocol.Message{Kind: protocol.KindLanSet}
		if a.opts.LanRelay == "" {
			errJSON, _ := json.Marshal("no LAN relay configured on this device")
			resp.Data = json.RawMessage(errJSON)
			return resp
		}
		a.SetLanEnabled(msg.Verdict == "on")
		return resp

	case protocol.KindLogs:
		if a.log == nil {
			return protocol.Message{Kind: protocol.KindLogs, Data: json.RawMessage("[]")}
		}
		f := eventlog.Filter{Type: msg.LogType, Grep: msg.Grep, Limit: int(msg.Limit)}
		if msg.Since != "" {
			if ts, err := time.Parse(time.RFC3339, msg.Since); err == nil {
				f.Since = ts
			}
		}
		events, err := a.log.Read(f)
		if err != nil {
			errJSON, _ := json.Marshal(err.Error())
			return protocol.Message{Kind: protocol.KindError, Data: json.RawMessage(errJSON)}
		}
		if events == nil {
			events = []eventlog.Event{}
		}
		data, _ := json.Marshal(events)
		return protocol.Message{Kind: protocol.KindLogs, Data: json.RawMessage(data)}

	default:
		return protocol.Message{Kind: protocol.KindError, Data: json.RawMessage(`"unknown RPC kind"`)}
	}
}

// pumpApprovalNotifs forwards console state changes as KindApprovalNotif frames
// until ctx is cancelled or a send fails.
func pumpApprovalNotifs(ctx context.Context, changes <-chan struct{}, svc *console.Service, send func(protocol.Message) error) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-changes:
			if !ok {
				return
			}
			// Push the full console State so the portal/SPA can re-render the
			// pending list, rules, and mode — including when the pending set
			// becomes empty (a resolved approval must clear from the UI).
			data, _ := json.Marshal(svc.State())
			if send(protocol.Message{Kind: protocol.KindApprovalNotif, Data: json.RawMessage(data)}) != nil {
				return
			}
		}
	}
}

// serveConsole handles an E2E console session with a portal. All writes go
// through a single write mutex so async approval notifications and RPC
// responses never interleave on the wire.
func (a *Agent) serveConsole(ctx context.Context, conn net.Conn) {
	// Derive a per-session context so the pump goroutine exits when this
	// session ends, regardless of the long-lived agent Run context.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wmu sync.Mutex
	send := func(m protocol.Message) error {
		wmu.Lock()
		defer wmu.Unlock()
		return protocol.WriteMessage(conn, m)
	}

	// Push approval notifications asynchronously.
	ch, unsub := a.console.Subscribe()
	defer unsub()
	go pumpApprovalNotifs(ctx, ch, a.console, send)

	// Speak the same framed protocol the controller/portal uses (the hello/OK
	// handshake in handleSession was framed too) — NOT raw json.Encoder.
	for {
		msg, err := protocol.ReadMessage(conn)
		if err != nil {
			return
		}
		resp := a.handleConsoleRPC(msg)
		// Audit who decided remotely (spec: approver=portal:<email>).
		if msg.Kind == protocol.KindDecide && msg.Approver != "" && resp.Kind != protocol.KindError {
			a.log.Append(eventlog.Event{Type: "connect", Detail: "remote decision " + msg.Verdict + " by " + msg.Approver})
		}
		_ = send(resp)
	}
}

// runConsolePrompt is a local CLI front-end for approval requests. It runs in a
// goroutine when the agent is launched in an interactive terminal. Decisions feed
// into the same queue as remote portal decisions; first answer wins.
func (a *Agent) runConsolePrompt(ctx context.Context) {
	ch, unsub := a.console.Subscribe()
	defer unsub()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ch:
			if !ok {
				return
			}
		}
		snap := a.console.State()
		for _, p := range snap.Pending {
			fmt.Printf("\n--- approval request ---\n")
			fmt.Printf("ID:  %s\n", p.ID)
			fmt.Printf("Cmd: %s\n", p.Cmd)
			fmt.Printf("Allow? [y] once  [a] remember dir  [g] remember global  [n] deny: ")
			var line string
			fmt.Scanln(&line)
			// console.Decide speaks the y/a/g/n vocabulary directly; anything
			// else (incl. empty) maps to deny.
			verdict := strings.TrimSpace(strings.ToLower(line))
			if verdict == "" {
				verdict = "n"
			}
			a.console.Decide(p.ID, verdict)
		}
	}
}
