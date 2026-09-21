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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"wanctl/internal/admission"
	"wanctl/internal/androidverb"
	"wanctl/internal/config"
	"wanctl/internal/console"
	"wanctl/internal/elevate"
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
	Version   string      // immutable release version reported to controllers
}

// Agent is a running controlled node.
type Agent struct {
	deviceID     string
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
	notifyMu     sync.RWMutex
	notifyPolicy agentNotifyPolicy
	notifyClient *http.Client

	sessMu           sync.Mutex
	sessions         map[string]*server.ShellSession
	workspaceMu      sync.Mutex
	workspaces       map[string]*workspace
	workspacesClosed bool
	// consoles counts live console sessions. An update that swapped the binary
	// under an owner who is mid-approval would drop the connection they are
	// answering on.
	consoles atomic.Int64
	jobs     *jobStore
	stdin    *bufio.Reader
	elevator *elevate.Manager

	// Owned goroutines. Every goroutine the agent starts is registered in wg
	// and takes its context from stopCtx, so Close can cancel them all and then
	// wait for the last one to return. Without that join the agent has no
	// moment where it is provably quiet: the control loop, a session handler or
	// the webhook reporter can still be writing to <config>/logs after the
	// caller believes the agent is finished.
	spawnMu sync.Mutex
	closing bool
	wg      sync.WaitGroup
	stopCtx context.Context
	stop    context.CancelFunc
}

// enter registers the calling goroutine as one the agent owns. It reports false
// if the agent is already closing, in which case the caller must return without
// doing any work: registering then would race Close's wg.Wait.
func (a *Agent) enter() bool {
	a.spawnMu.Lock()
	defer a.spawnMu.Unlock()
	if a.closing {
		return false
	}
	a.wg.Add(1)
	return true
}

// leave releases the registration taken by enter.
func (a *Agent) leave() { a.wg.Done() }

// spawn runs fn on a goroutine the agent owns, so Close joins it.
func (a *Agent) spawn(fn func()) {
	if !a.enter() {
		return
	}
	go func() {
		defer a.leave()
		fn()
	}()
}

// shutdown is the context cancelled by Close. An Agent assembled directly by a
// unit test (no New) has no shutdown context and never stops on its own, which
// is the behaviour those tests already relied on.
func (a *Agent) shutdown() context.Context {
	if a.stopCtx == nil {
		return context.Background()
	}
	return a.stopCtx
}

// runContext derives ctx so it is cancelled by the caller's context or by
// Close, whichever comes first.
func (a *Agent) runContext(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(a.shutdown(), cancel)
	return ctx, func() { stop(); cancel() }
}

// Close shuts the agent down and waits for the goroutines it owns to return:
// the control loop, live session handlers, the notify-policy loop and any
// in-flight webhook report. It is idempotent, and a closed agent
// stays closed (Run on one returns immediately).
//
// Production shutdown is process exit, so nothing on the daemon path has to
// call this; what it buys is a point where the agent is provably done touching
// its config directory. Tests need exactly that — an agent that appends to
// <config>/logs/events.jsonl after the test's last read makes t.TempDir's
// RemoveAll fail with "directory not empty" (#35).
func (a *Agent) Close() {
	a.spawnMu.Lock()
	a.closing = true
	a.spawnMu.Unlock()
	if a.stop != nil {
		a.stop()
	}
	a.closeWorkspaces()
	a.wg.Wait()
}

// sleepCtx waits for d and reports whether it elapsed; a cancelled context ends
// the wait early so backoffs never outlive a shutdown.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// New constructs an Agent with loaded identity, controller allow-list, and
// policy engine. The approver is the queue-backed console service; a local CLI
// terminal and/or a connected portal can both feed decisions into the same
// queue. Headless + no portal connected means the 60 s timeout deny fires.
func New(opts Options) (*Agent, error) {
	deviceID, err := transport.LoadOrCreateDeviceID()
	if err != nil {
		return nil, err
	}
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
	if opts.Version == "" {
		opts.Version = "dev"
	}
	// opts.Mode is deliberately left empty when no --mode was given: policy.Open
	// reads an empty mode as "use the persisted one", which is what lets a
	// portal-side switch survive a restart and why `service install` omits the
	// flag on purpose. Defaulting it to ModeNormal here made that branch
	// unreachable, so every flag-less restart quietly reverted the device to
	// normal while main.go's flag help promised the opposite.
	if opts.Name == "" {
		opts.Name = defaultDeviceName()
	}
	inst, err := newInstanceID()
	if err != nil {
		return nil, err
	}
	engine, err := policy.Open("rules.json", opts.Mode)
	if err != nil {
		return nil, err
	}
	// Spilled command output from a previous life is not state this agent
	// needs, and a device that was driven hard and then restarted should not
	// carry the whole pile forward until each file ages out on its own.
	server.SweepSpills()
	// A PowerShell device needs prefix rules matched against PowerShell's own
	// evaluation syntax, which the POSIX command parser cannot see
	// (audit 2026-08-28, SEC-D1-02).
	engine.SetPowerShell(isPowerShell(opts.Shell))
	logger, err := eventlog.Open("events.jsonl")
	if err != nil {
		return nil, err
	}
	a := &Agent{
		deviceID: deviceID, id: id, known: known, portalAdmins: portalAdmins, opts: opts, engine: engine, log: logger,
		inst:     inst,
		sessions: map[string]*server.ShellSession{}, jobs: newJobStore(), stdin: bufio.NewReader(os.Stdin),
		elevator: elevate.ConfigureDefault(configDirOrEmpty(), os.Getenv),
	}
	// Shutdown context for everything the agent starts; Close cancels it and
	// then joins those goroutines.
	a.stopCtx, a.stop = context.WithCancel(context.Background())
	a.console = console.New(engine, logger, console.Info{
		Device: opts.Name, Fingerprint: id.Fingerprint, Relay: opts.RelayURL,
		Platform: runtime.GOOS, ADBPair: runtime.GOOS == "android",
	})
	a.console.SetTrustedSource(a.trustedControllers)
	a.console.SetPendingHook(a.notifyApproval)
	a.console.SetPairingHook(a.notifyPairing)
	a.jobs.onDone = func(command, cwd string, code int) {
		a.notifyExecFinished(command, cwd, "", code)
	}
	// Queue-backed approver: the remote portal and/or the local CLI terminal
	// feed decisions into the same queue. Headless + no portal -> timeout deny.
	//
	// Bypass is NOT wired in here. It is a runtime property of the engine (the
	// portal can flip it mid-flight), and both gates already short-circuit on
	// engine.Mode() before consulting the approver. Pinning an AllowApprover at
	// construction time would survive the portal turning bypass off, auto-allowing
	// everything while the audit log claimed a human said "approved".
	a.appr = a.console
	return a, nil
}

// configDirOrEmpty is where the adb channel keeps its key. An error here is
// not fatal to the agent: it only means the adb channel cannot store a key and
// will report itself unavailable, which is the right outcome for a device whose
// config dir is unreadable anyway.
func configDirOrEmpty() string {
	dir, err := transport.ConfigDir()
	if err != nil {
		return ""
	}
	return dir
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
func (a *Agent) gateDataCapability(cap dataCapability, peerFP string, checks ...func() bool) (bool, string) {
	switch cap {
	case capabilityReadEventLog:
		return a.gate(policy.Request{Kind: policy.KindLogs, Peer: peerFP}, checks...)
	default:
		return false, "unsupported-capability"
	}
}

// gate authorizes a request: bypass/pre-approved pass; otherwise ask the
// approver and optionally remember a rule. Returns whether the op may proceed
// and a short decision string for the audit log.
func (a *Agent) gate(req policy.Request, checks ...func() bool) (bool, string) {
	// Bypasses, not Mode()==bypass: an elevated command rides the blanket allow
	// only on a device whose elevation channel is also switched on. Two opt-ins,
	// both off by default, are the consent (policy.KindExecElevated).
	if a.engine.Bypasses(req.Kind, a.elevationEnabled()) {
		return true, "bypass"
	}
	if a.engine.Allowed(req) {
		return true, "pre-approved"
	}
	a.apprMu.Lock()
	appr := a.appr
	a.apprMu.Unlock()
	d := appr.Ask(req)
	if !checksPass(checks) {
		return false, "delegation inactive"
	}
	if !d.Allow {
		return false, "denied"
	}
	if d.Remember {
		a.engine.Add(policy.RuleFor(req, d.Scope))
		return true, "remembered:" + string(d.Scope)
	}
	return true, "approved"
}

// elevationEnabled reports whether this device's elevation channel is switched
// on — the 提权通道 switch in the Android app, off by default. It is the second
// of the two opt-ins that let bypass mode cover an elevated command; an agent
// built without an elevator has no channel and therefore no second opt-in.
func (a *Agent) elevationEnabled() bool {
	return a.elevator != nil && a.elevator.Enabled()
}

// gateFile returns the policy root that must constrain the actual filesystem
// open. A one-shot approval is restricted to the requested file's parent;
// global and bypass decisions use an empty root, meaning the filesystem volume.
func (a *Agent) gateFile(req policy.Request, checks ...func() bool) (bool, string, string) {
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
	if !checksPass(checks) {
		return false, "delegation inactive", ""
	}
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
func (a *Agent) Run(ctx context.Context) error {
	// The caller starts Run, but the agent owns it: Close must join the control
	// loop too, or the loop can still accept a session — and log it — after
	// everything else has stopped.
	if !a.enter() {
		return nil
	}
	defer a.leave()
	ctx, cancel := a.runContext(ctx)
	defer cancel()

	a.spawn(func() { a.runNotifyPolicy(ctx) })
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
	if err := enc.Encode(map[string]string{"op": "register", "device": a.DeviceID(), "device_id": a.DeviceID(), "name": a.opts.Name, "fingerprint": a.id.Fingerprint, "inst": a.inst, "delegation": "1"}); err != nil {
		return err
	}
	fmt.Printf("wanctl agent %q online via %s\n  fingerprint: %s\n", a.opts.Name, a.opts.RelayURL, a.id.Fingerprint)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		a.spawn(func() { a.runConsolePrompt(ctx) })
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
		if msg.Op == "open" && msg.ValidFor(a.DeviceID()) {
			a.spawn(func() { a.serveSession(ctx, msg) })
		}
	}
}

// serveSession opens the per-session WebSocket on the relay and serves it.
func (a *Agent) serveSession(ctx context.Context, open sessionauth.Open) {
	url := strings.TrimRight(a.opts.RelayURL, "/") + open.URL
	nc, _, err := wsconn.Dial(ctx, url, admission.Header(a.opts.Token))
	if err != nil {
		return
	}
	a.handleSession(ctx, nc, open)
}

// handleSession completes the server-side handshake and serves requests on an
// already-established transport conn (WebSocket or HTTP).
// rejectHandshakeLinger bounds how long a rejected session stays open waiting
// for the controller to read its reason. The controller closes as soon as it
// has the message, so this is only ever spent on a controller that stopped
// reading.
const rejectHandshakeLinger = 3 * time.Second

// rejectHandshake sends a handshake rejection and then waits for the controller
// to close, instead of returning straight into the caller's deferred Close.
//
// The relay pipes the two sides and tears BOTH ends down as soon as either
// direction ends. Closing right after the write therefore races our own
// rejection through that teardown: the bytes are handed to the controller's
// transport but the connection can be closed before they are delivered — on the
// HTTP carrier, before the controller has even polled for them. The controller
// then reports EOF, and the caller sees a connection failure instead of
// "capability denied" or a pairing URL it could act on.
//
// Waiting for the peer's own close keeps the teardown ordered without relying on
// flush semantics that differ between the WebSocket and HTTP carriers.
// The deadline is enforced by closing the connection rather than with
// SetReadDeadline: the HTTP carrier's conn accepts deadlines and ignores them
// (internal/httpconn), so a controller that neither reads nor closes would pin
// this goroutine forever on exactly the transport where the bug shows up.
func rejectHandshake(conn net.Conn, msg protocol.Message) {
	if err := protocol.WriteMessage(conn, msg); err != nil {
		return
	}
	stop := time.AfterFunc(rejectHandshakeLinger, func() { conn.Close() })
	defer stop.Stop()
	_, _ = io.Copy(io.Discard, conn)
}

// refuse rejects a session and records it. The event log only ever contained
// connections that succeeded, so every refusal — an unpaired controller, a
// non-admin asking for the console, an anonymous pairing attempt — left the
// device with no trace at all. Denials are the half worth keeping: they are what
// you read when something cannot connect, and what would show someone probing.
func (a *Agent) refuse(conn net.Conn, fp, name, decision string, msg protocol.Message, scopes ...sessionAudit) {
	if a.log != nil {
		a.logSessionEvent(firstAudit(scopes), eventlog.Event{Type: "connect", PeerFP: fp, PeerName: name, Decision: decision, Detail: msg.Reason})
	}
	rejectHandshake(conn, msg)
}

func (a *Agent) handleSession(ctx context.Context, nc net.Conn, auth sessionauth.Open) {
	audit := auditSession(auth)
	if auth.GrantID != "" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, auth.ExpiresAt)
		defer cancel()
		defer wsconn.CloseOnCancel(ctx, nc)()
	}
	conn, fp, err := transport.ServerHandshake(ctx, nc, a.id)
	if err != nil {
		return
	}
	defer conn.Close()
	// Every read below parks until the controller says something. Cancelling the
	// run context (SIGTERM, Close) has to reach them, or a shutdown waits on a
	// peer that may never write again.
	defer wsconn.CloseOnCancel(ctx, conn)()

	hello, err := protocol.ReadMessage(conn)
	if err != nil {
		return
	}
	if hello.Kind != protocol.KindHello && hello.Kind != protocol.KindConsoleHello && hello.Kind != protocol.KindWorkspaceHello {
		return
	}
	if !auth.ValidFor(a.DeviceID()) {
		a.refuse(conn, fp, hello.Name, "rejected:session", protocol.Message{Kind: protocol.KindReject, Reason: "invalid relay session capabilities"}, audit)
		return
	}
	if auth.GrantID != "" && (auth.ControllerFingerprint != fp || !a.delegationActive(ctx, auth, fp)) {
		a.refuse(conn, fp, hello.Name, "rejected:delegation", protocol.Message{Kind: protocol.KindReject, Reason: "delegation is inactive or controller fingerprint does not match"}, audit)
		return
	}
	// Pairing grants a controller permission to submit device operations; it
	// must not grant the control-plane capability to approve those operations,
	// change rules, or enable bypass mode. Only the enrolled portal identity is
	// a console administrator, and the relay session must independently carry
	// the console capability. An empty administrator set therefore fails closed.
	if hello.Kind == protocol.KindConsoleHello {
		if !auth.Capabilities.Has(sessionauth.Console) {
			a.refuse(conn, fp, hello.Name, "rejected:capability", protocol.Message{Kind: protocol.KindReject, Reason: "session capability denied: console"}, audit)
			return
		}
		if a.portalAdmins == nil || !a.portalAdmins.Contains(fp) {
			a.refuse(conn, fp, hello.Name, "rejected:not-console-admin", protocol.Message{
				Kind:   protocol.KindReject,
				Reason: "controller is not authorized as this device's console administrator",
			}, audit)
			return
		}
	}
	if a.mustIdentify(hello.Kind, fp, hello.Label) {
		a.refuse(conn, fp, hello.Name, "rejected:unlabeled", protocol.Message{Kind: protocol.KindReject, Reason: unlabeledReason}, audit)
		return
	}
	// Authorize (TOFU / pre-trusted portal key) and reply OK for BOTH exec and
	// console sessions BEFORE serving — the controller/portal blocks on this OK,
	// and a console session must be gated by the same trust check as an exec one.
	if !a.authorize(fp, hello.Name, hello.Label, audit) {
		pairingURL := a.pairingURL(fp, hello.Name, hello.Label)
		reason := "device has not paired this controller — ask the user to approve"
		if pairingURL == "" {
			reason = "device owner must approve on the device console (or set WANCTL_PORTAL on the agent to get a clickable pairing link)"
		}
		a.refuse(conn, fp, hello.Name, "rejected:unpaired", protocol.Message{
			Kind:       protocol.KindReject,
			Reason:     reason,
			PairingURL: pairingURL,
		}, audit)
		return
	}
	if hello.Kind == protocol.KindWorkspaceHello {
		audit.workspaceCheck = func() bool { return a.workspaceSessionActive(ctx, auth, fp) }
		if !audit.workspaceCheck() {
			a.refuse(conn, fp, hello.Name, "rejected:workspace-access", protocol.Message{Kind: protocol.KindReject, Reason: "workspace connection authorization unavailable; relay and agent must support reusable workspaces"}, audit)
			return
		}
	}
	protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindOK, Name: a.opts.Name, WorkspaceReuse: hello.Kind == protocol.KindWorkspaceHello})
	a.logSessionEvent(audit, eventlog.Event{Type: "connect", PeerFP: fp, PeerName: hello.Name, Decision: "accepted"})
	if hello.Kind == protocol.KindConsoleHello {
		a.serveConsole(ctx, conn)
		return
	}
	var check func() bool
	if auth.GrantID != "" {
		check = func() bool { return a.delegationActive(ctx, auth, fp) }
	}
	a.serveAuthorized(conn, fp, hello.Name, auth.Capabilities, check, audit)
}

// unlabeledReason tells the controller how to become answerable, because the
// person who would otherwise be asked cannot.
const unlabeledReason = "controller did not identify itself — set a label (`wanctl label \"<who you are>\"`, or WANCTL_LABEL) and retry; a pairing request without one is not raised to the device owner"

// unlabeledPairing reports whether this hello would raise a pairing decision
// nobody can actually make. An unknown controller with no self-description
// reaches the owner as "trust SHA256:… from bogon?", and prompts that carry no
// answerable information get clicked through — which makes the pairing gate
// worse than useless. Already-trusted controllers and --yes are unaffected.
func (a *Agent) unlabeledPairing(fp, label string) bool {
	return !a.known.Has(fp) && !a.opts.AutoYes && strings.TrimSpace(label) == ""
}

// mustIdentify decides whether this hello has to carry a self-description.
// Console sessions are exempt: they have already passed the portal-admin check,
// which is a stronger statement than a label. Without the exemption a portal
// fingerprint added to portal_admins.json while the agent is running — not yet
// mirrored into known_clients — would be told to introduce itself, locking the
// portal out of the very device someone is trying to repair.
func (a *Agent) mustIdentify(helloKind, fp, label string) bool {
	if helloKind == protocol.KindConsoleHello {
		return false
	}
	return a.unlabeledPairing(fp, label)
}

func (a *Agent) authorize(fp, name, label string, scopes ...sessionAudit) bool {
	if a.known.Has(fp) {
		a.known.Touch(fp)
		return true
	}
	if a.opts.AutoYes {
		a.known.AddLabeled(fp, name, label)
		fmt.Printf("[auto-trust] new controller %q paired: %s\n", name, fp)
		// --yes skips the prompt, not the record: the admission is permanent
		// (known_clients.json) and the owner who opted in still gets to ask
		// "who has been admitted, and when" through `wanctl logs --type trust`
		// instead of a stdout line nobody reads.
		a.logSessionEvent(firstAudit(scopes), eventlog.Event{Type: "trust", PeerFP: fp, PeerName: name, Detail: label, Decision: "auto-trust"})
		a.notifyTrustChanged(fp, name, "granted")
		return true
	}
	// Surface the pairing request to a connected front-end (the portal web
	// console) and block for a human's trust decision. A headless agent with no
	// portal connected denies (pre-trust with --portal-fps or --yes instead).
	var paired bool
	if firstAudit(scopes).grantID != "" {
		paired = a.console.AskPairNonBlocking(fp, name, label)
	} else {
		paired = a.console.AskPair(fp, name, label)
	}
	if paired {
		a.known.AddLabeled(fp, name, label)
		fmt.Printf("[paired] controller %q trusted via console: %s\n", name, fp)
		a.logSessionEvent(firstAudit(scopes), eventlog.Event{Type: "trust", PeerFP: fp, PeerName: name, Detail: label, Decision: "console"})
		a.notifyTrustChanged(fp, name, "granted")
		return true
	}
	return false
}

// pairingURL builds the portal URL a user clicks to trust this controller. The
// AI surfaces it verbatim in its reply ("ask the user to click this link"); the
// SPA's #pair route reads device/fp/label and shows a confirmation card.
func (a *Agent) pairingURL(fp, name, label string) string {
	portal, _ := config.Setting("portal")
	if portal == "" {
		return ""
	}
	q := url.Values{}
	q.Set("device", a.DeviceID())
	q.Set("fp", fp)
	if name != "" {
		q.Set("name", name)
	}
	if label != "" {
		q.Set("label", label)
	}
	return strings.TrimRight(portal, "/") + "/#pair?" + q.Encode()
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

// peerRead is one control message read from the controller, or the error that
// ended the stream.
type peerRead struct {
	msg protocol.Message
	err error
}

// watchPeer reads the next control message while a command is running, so the
// device notices a controller that went away (Ctrl-C, dropped link) or that
// asks outright to abort: either one cancels the command's context, and the
// per-platform cancel hook then kills the shell and everything under it.
//
// The read it starts owns the connection's read side, so its result is handed
// back on the returned channel — the request loop must take its next message
// from there rather than reading the connection itself, or the two reads race.
func watchPeer(conn io.Reader, cancel context.CancelFunc) <-chan peerRead {
	ch := make(chan peerRead, 1)
	go func() {
		m, err := protocol.ReadMessage(conn)
		ch <- peerRead{msg: m, err: err}
		if err != nil || m.Kind == protocol.KindCancel {
			cancel()
		}
	}()
	return ch
}

func (a *Agent) serve(conn *tls.Conn, fp, peerName string, caps sessionauth.Capabilities) {
	a.serveAuthorized(conn, fp, peerName, caps, nil)
}

func (a *Agent) serveAuthorized(conn *tls.Conn, fp, peerName string, caps sessionauth.Capabilities, check func() bool, scopes ...sessionAudit) {
	audit := firstAudit(scopes)
	// Set while a read started by doExec is still in flight; the next request
	// comes from it (see watchPeer).
	var pending <-chan peerRead
	for {
		var m protocol.Message
		var err error
		if pending != nil {
			r := <-pending
			pending, m, err = nil, r.msg, r.err
		} else {
			m, err = protocol.ReadMessage(conn)
		}
		if err != nil {
			return
		}
		if audit.workspaceCheck != nil && (m.Kind != protocol.KindWorkspace || !audit.workspaceCheck()) {
			a.logSessionEvent(audit, rejectedRequestEvent(fp, peerName, m, "workspace access inactive"))
			rejectHandshake(conn, protocol.Message{Kind: protocol.KindReject, Reason: "workspace connection access inactive or request outside workspace protocol"})
			return
		}
		if check != nil && !check() {
			a.logSessionEvent(audit, rejectedRequestEvent(fp, peerName, m, "delegation inactive"))
			protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindReject, Reason: "delegation inactive"})
			return
		}
		if check != nil && (m.Kind == protocol.KindWorkspace || m.Kind == protocol.KindExecAsync || m.Kind == protocol.KindExecPoll || (m.Kind == protocol.KindExec && !m.OneShot)) {
			a.logSessionEvent(audit, rejectedRequestEvent(fp, peerName, m, "delegated execution requires a synchronous one-shot command"))
			protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindReject, Reason: "delegated execution requires a synchronous one-shot command"})
			continue
		}
		if m.Kind == protocol.KindWorkspace && a.handleWorkspace(conn, fp, peerName, &m, caps, audit) {
			continue
		}
		if required := requiredCapability(m.Kind); required != 0 && !caps.Has(required) {
			a.logSessionEvent(audit, rejectedRequestEvent(fp, peerName, m, "session capability denied: "+required.String()))
			protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindReject, Reason: "session capability denied: " + required.String()})
			continue
		}
		switch m.Kind {
		case protocol.KindExec:
			pending = a.doExecAuthorized(conn, fp, peerName, m, audit, check)
		case protocol.KindCancel:
			// Nothing is running on this stream: a cancel that lost the race
			// with its own command finishing is not a protocol error.
		case protocol.KindExecAsync:
			a.doExecAsync(conn, fp, peerName, m)
		case protocol.KindExecPoll:
			a.doExecPoll(conn, m)
		case protocol.KindLogs:
			ok, decision := a.gateDataCapability(capabilityReadEventLog, fp, check)
			if ok && !checksPass([]func() bool{check, audit.workspaceCheck}) {
				ok, decision = false, "delegation inactive"
			}
			a.logSessionEvent(audit, eventlog.Event{
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
		case protocol.KindStatus:
			if audit.grantID != "" {
				a.logSessionEvent(audit, eventlog.Event{Type: "status", PeerFP: fp, PeerName: peerName, Decision: "accepted"})
			}
			protocol.WriteMessage(conn, a.status())
		case protocol.KindFilePut:
			ok, decision, root := a.gateFile(policy.Request{Kind: policy.KindWrite, Path: m.Path, Peer: fp}, check, audit.workspaceCheck)
			if ok && !checksPass([]func() bool{check, audit.workspaceCheck}) {
				ok, decision = false, "delegation inactive"
			}
			a.logSessionEvent(audit, eventlog.Event{Type: "file", PeerFP: fp, PeerName: peerName, Detail: "PUT " + m.Path, Decision: decision})
			if !ok {
				protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindReject, Reason: "write denied by device policy: " + m.Path})
				continue
			}
			server.HandleFilePut(conn, m, root)
		case protocol.KindFileGet:
			ok, decision, root := a.gateFile(policy.Request{Kind: policy.KindRead, Path: m.Path, Peer: fp}, check, audit.workspaceCheck)
			if ok && !checksPass([]func() bool{check, audit.workspaceCheck}) {
				ok, decision = false, "delegation inactive"
			}
			a.logSessionEvent(audit, eventlog.Event{Type: "file", PeerFP: fp, PeerName: peerName, Detail: "GET " + m.Path, Decision: decision})
			if !ok {
				protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindReject, Reason: "read denied by device policy: " + m.Path})
				continue
			}
			server.HandleFileGet(conn, m, root)
		case protocol.KindFileRead:
			// Gated exactly like file_get: a read is a read, whether the
			// controller wants the whole file or twenty lines of it.
			ok, decision, root := a.gateFile(policy.Request{Kind: policy.KindRead, Path: m.Path, Peer: fp}, check, audit.workspaceCheck)
			if ok && !checksPass([]func() bool{check, audit.workspaceCheck}) {
				ok, decision = false, "delegation inactive"
			}
			a.logSessionEvent(audit, eventlog.Event{Type: "file", PeerFP: fp, PeerName: peerName, Detail: "READ " + m.Path, Decision: decision})
			if !ok {
				protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindReject, Reason: "read denied by device policy: " + m.Path})
				continue
			}
			server.HandleFileRead(conn, m, root)
		case protocol.KindFileEdit:
			// Gated exactly like file_put. An edit rewrites the file, so the
			// grant it needs is the write grant, not a lesser one for touching
			// only part of the contents.
			ok, decision, root := a.gateFile(policy.Request{Kind: policy.KindWrite, Path: m.Path, Peer: fp}, check, audit.workspaceCheck)
			if ok && !checksPass([]func() bool{check, audit.workspaceCheck}) {
				ok, decision = false, "delegation inactive"
			}
			a.logSessionEvent(audit, eventlog.Event{Type: "file", PeerFP: fp, PeerName: peerName, Detail: "EDIT " + m.Path, Decision: decision})
			if !ok {
				protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindReject, Reason: "write denied by device policy: " + m.Path})
				continue
			}
			server.HandleFileEdit(conn, m, root)
		case protocol.KindFileWrite:
			// Gated exactly like file_put, for the same reason as file_edit:
			// what comes out of it is a whole file with the controller's
			// content in it, which is a write however small the content was.
			ok, decision, root := a.gateFile(policy.Request{Kind: policy.KindWrite, Path: m.Path, Peer: fp}, check, audit.workspaceCheck)
			if ok && !checksPass([]func() bool{check, audit.workspaceCheck}) {
				ok, decision = false, "delegation inactive"
			}
			a.logSessionEvent(audit, eventlog.Event{Type: "file", PeerFP: fp, PeerName: peerName, Detail: "WRITE " + m.Path, Decision: decision})
			if !ok {
				protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindReject, Reason: "write denied by device policy: " + m.Path})
				continue
			}
			server.HandleFileWrite(conn, m, root)
		default:
			protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: "unknown request: " + m.Kind})
			return
		}
	}
}

func (a *Agent) status() protocol.Message {
	return protocol.Message{
		Kind: protocol.KindStatus, Name: a.opts.Name, Version: a.opts.Version, ConsoleMode: string(a.engine.Mode()),
	}
}

func requiredCapability(kind string) sessionauth.Capabilities {
	switch kind {
	case protocol.KindExec, protocol.KindExecAsync, protocol.KindExecPoll:
		return sessionauth.Exec
	case protocol.KindFileGet, protocol.KindFileRead:
		return sessionauth.Read
	case protocol.KindFilePut, protocol.KindFileEdit, protocol.KindFileWrite:
		return sessionauth.Write
	case protocol.KindLogs:
		return sessionauth.Logs
	default:
		return 0
	}
}

// doExec runs one command for a controller. It returns the in-flight read that
// watched for the controller leaving, so the request loop can take its next
// message from there; nil means the loop owns the connection again.
func (a *Agent) doExec(conn *tls.Conn, fp, peerName string, m protocol.Message, checks ...func() bool) <-chan peerRead {
	return a.doExecAuthorized(conn, fp, peerName, m, sessionAudit{}, checks...)
}

func (a *Agent) doExecAuthorized(conn *tls.Conn, fp, peerName string, m protocol.Message, audit sessionAudit, checks ...func() bool) <-chan peerRead {
	kind := policy.KindExec
	// A desktop capture asks for elevation it will not use: the controller has
	// to request it so an Android device can honour it, and no laptop has a
	// channel to run it through. Gating it as elevated would make looking at a
	// screen harder than running the capture tool by hand through exec, which
	// is the same capability by a longer road. Android keeps the elevated gate,
	// because there it really does need su or adb.
	if m.Elevate && !server.IsDesktopCapture(m.Command) {
		kind = policy.KindExecElevated
	}
	// An unparseable --via is rejected before the approval prompt, not after:
	// nobody should be asked to approve a command that cannot run anyway.
	var via elevate.Kind
	if m.Elevate && m.Via != "" {
		parsed, err := elevate.ParseKind(m.Via)
		if err != nil {
			protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: err.Error()})
			return nil
		}
		via = parsed
	}

	ok, decision := a.gate(policy.Request{Kind: kind, Cmd: m.Command, Cwd: m.Cwd, Peer: fp, Via: string(via)}, checks...)
	if ok && !checksPass(checks) {
		ok, decision = false, "delegation inactive"
	}
	if !ok {
		a.logSessionEvent(audit, eventlog.Event{Type: "exec", PeerFP: fp, PeerName: peerName, Detail: m.Command, Cwd: m.Cwd, Decision: decision, Via: string(via)})
		reason := "command denied by device policy: " + m.Command
		if kind == policy.KindExecElevated {
			// CommandPattern, not CommandLabel: this text is read by whoever
			// will go and write the rule, so it carries the whole token rather
			// than the abbreviation a card shows.
			reason = "elevated command denied by device policy: " + policy.CommandPattern(m.Command) +
				" (elevated commands need their own rule; bypass mode does not cover them" +
				" until this device's elevation channel is switched on)"
		}
		protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindReject, Reason: reason})
		return nil
	}

	// The command runs under a context tied to this request. A controller that
	// goes away mid-command (Ctrl-C) or sends a cancel frame cancels it, and
	// the per-platform cancel hook kills the shell and its children instead of
	// leaving an orphan running to completion on the device (#37).
	//
	// A persistent session cancels the same way, and the session goes with it:
	// the shell is fed through stdin, so stopping one command in it and keeping
	// the rest is not something the device can promise (#46, ADR 0011).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pending := watchPeer(conn, cancel)

	// The output goes to the controller and, past the size it asked about, into
	// a file here as well, so a truncated answer can still say where the rest
	// is. A controller that names no threshold gets exactly today's behaviour
	// and no file.
	spill := server.NewSpill(server.FrameWriter(conn, protocol.FrameStdout), m.SpillAfter)
	out := io.Writer(spill)
	var code int
	var err error
	var ranVia elevate.Kind
	switch {
	case m.Elevate:
		// A desktop capture is the same verb through the same gate, with no
		// elevation channel to run it through — there is none on a laptop, and
		// none is needed. Checked before the Android verbs so that a device
		// which is not Android never reaches the elevator at all.
		if handled, sCode, sErr := server.RunScreenshot(ctx, m.Command, out); handled {
			code, err = sCode, sErr
			break
		}
		// Structured verbs first: they are elevated commands with a nicer
		// spelling, so they go through the same channel and the same gate that
		// already ran above.
		handled, vVia, vCode, vErr := androidverb.Dispatch(ctx, m.Command, via, a.elevator, out)
		if handled {
			ranVia, code, err = vVia, vCode, vErr
		} else {
			ranVia, code, err = a.elevator.Run(ctx, via, m.Command, m.Cwd, out)
		}
		if err != nil {
			// A channel that could not be selected has not run anything, so
			// this is a refusal to act rather than a failed command. Say which
			// it is: the caller must not read it as "ran, and failed".
			a.logSessionEvent(audit, eventlog.Event{Type: "exec", PeerFP: fp, PeerName: peerName, Detail: m.Command, Cwd: m.Cwd, Decision: decision, Via: string(via)})
			spill.Close()
			protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: err.Error()})
			return pending
		}
	default:
		// adb-pair is unelevated on purpose: it is how the elevation channel
		// gets set up in the first place (see runADBPair).
		if handled, pairCode, pairErr := a.runADBPair(m.Command, out); handled {
			code, err = pairCode, pairErr
		} else if handled, builtinCode, builtinErr := server.RunBuiltin(m.Command, out); handled {
			code, err = builtinCode, builtinErr
		} else if verb, needsElevate := androidverb.NeedsElevation(runtime.GOOS, m.Command); needsElevate {
			// The verb dispatcher only runs on the elevated path, so without
			// --elevate this would reach the shell and come back as exit 127
			// with the device blamed for a flag the caller left off (#71).
			// Android only: on a desktop these are just words, and one of them
			// may well name a program the caller means to run.
			code, err = -1, fmt.Errorf("%q is a wanctl verb and only runs elevated: add --elevate (and turn on 提权通道 on the device)", verb)
		} else if m.OneShot {
			code, err = server.RunOneShotContext(ctx, a.opts.Shell, m.Command, m.Cwd, out)
		} else {
			var serr error
			code, err, serr = a.execInSession(ctx, fp, m, out)
			if serr != nil {
				spill.Close()
				protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: serr.Error()})
				return pending
			}
		}
	}
	if err != nil {
		if ctx.Err() != nil && !errors.Is(err, server.ErrSessionCancelled) {
			// Say who ended it: the audit line and a controller that is still
			// listening must not read this as the command itself failing. A
			// session cancellation already says so, and may carry a kill that
			// failed, so it is passed through untouched.
			err = fmt.Errorf("command cancelled by the controller")
		}
		a.logSessionEvent(audit, eventlog.Event{Type: "exec", PeerFP: fp, PeerName: peerName, Detail: m.Command, Cwd: m.Cwd, Decision: decision, Via: string(ranVia)})
		if code == 0 {
			code = -1
		}
		a.notifyExecFinished(m.Command, m.Cwd, peerName, code)
		// A command that failed with a huge output is exactly when knowing
		// where the rest of it is matters most, so the error frame carries the
		// spill too.
		spillPath, spilled, kept := spill.Close()
		protocol.WriteMessage(conn, protocol.Message{
			Kind: protocol.KindError, Reason: err.Error(),
			Path: spillPath, Size: spilled, SpillKept: kept,
		})
		return pending
	}
	a.logSessionEvent(audit, eventlog.Event{Type: "exec", PeerFP: fp, PeerName: peerName, Detail: m.Command, Cwd: m.Cwd, Decision: decision, Exit: &code, Via: string(ranVia)})
	a.notifyExecFinished(m.Command, m.Cwd, peerName, code)
	spillPath, spilled, kept := spill.Close()
	protocol.WriteMessage(conn, protocol.Message{
		Kind: protocol.KindExit, Code: code, ElevatedVia: string(ranVia),
		Path: spillPath, Size: spilled, SpillKept: kept,
	})
	return pending
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
	b, err := config.RelayHTTPOrigin(relayURL)
	if err != nil {
		return strings.TrimRight(relayURL, "/")
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
	qv := url.Values{"device": {a.DeviceID()}}
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
	q := url.Values{"device": {a.DeviceID()}, "device_id": {a.DeviceID()}, "name": {a.opts.Name}, "fp": {a.id.Fingerprint}, "inst": {a.inst}, "delegation": {"1"}}.Encode()
	pollURL := base + "/h/poll?" + q
	// Registration lives or dies by this loop: the relay keeps a device listed
	// only while its polls keep arriving. Until 2026-08-07 every failure here
	// was swallowed — sleep two seconds, try again, say nothing — so an agent
	// whose polls stopped working looked, from the device, exactly like one
	// that was fine: process alive, no errors, "online" printed at startup and
	// never contradicted. From the controller it had simply vanished. That is
	// the least debuggable state a daemon can be in, and it cost an afternoon
	// on an Android tablet that had roamed onto a different Wi-Fi.
	//
	// So: say the first failure out loud, then throttle to roughly once a
	// minute (a two-second backoff means ~30 attempts), and say when it comes
	// back. Enough to see the pattern in a log; not enough to fill a disk
	// overnight on a device that is simply off the network.
	failures := 0
	report := func(format string, args ...any) {
		if failures == 1 || failures%30 == 0 {
			fmt.Fprintf(os.Stderr, "wanctl: "+format+" (%d consecutive)\n", append(args, failures)...)
		}
	}
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
			failures++
			report("relay poll failed: %v", err)
			sleepCtx(ctx, 2*time.Second) // backoff then re-poll (the loop top handles a cancelled ctx)
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
		// Any other non-2xx keeps the loop running — a relay restart or a
		// gateway hiccup should not take the agent down — but it is no longer
		// indistinguishable from success.
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			resp.Body.Close()
			failures++
			report("relay poll returned %s", resp.Status)
			sleepCtx(ctx, 2*time.Second)
			continue
		}
		if failures > 0 {
			fmt.Fprintf(os.Stderr, "wanctl: relay poll recovered after %d consecutive failures\n", failures)
			failures = 0
		}
		var msg sessionauth.Open
		json.NewDecoder(resp.Body).Decode(&msg)
		resp.Body.Close()
		if msg.ValidFor(a.DeviceID()) {
			a.spawn(func() { a.serveSessionHTTP(ctx, base, msg) })
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

// execInSession runs a command on this controller's persistent session. serr is
// set only when no session could be built at all; anything the command itself
// reported comes back in err.
//
// A session can be cancelled and dropped by another request between this one
// acquiring it and running, which used to surface as "session closed" for a
// command that never reached the device. The session says so precisely, and
// that one error is answered by acquiring a fresh session and running once.
// Nothing else is retried: every other failure leaves open the possibility that
// the command did run, and running it twice is worse than reporting it once.
func (a *Agent) execInSession(ctx context.Context, fp string, m protocol.Message, out io.Writer) (code int, err, serr error) {
	for attempt := 0; ; attempt++ {
		sess, sessErr := a.session(fp)
		if sessErr != nil {
			return -1, nil, sessErr
		}
		code, err = sess.ExecInDirContext(ctx, m.Command, m.Cwd, out)
		if sess.Closed() {
			// Cancelling a session command destroys the session by design
			// (#46). Drop it so the next command on this target builds a fresh
			// shell rather than finding a dead one.
			a.dropSession(fp, sess)
		}
		if errors.Is(err, server.ErrSessionUnusable) && attempt == 0 {
			continue
		}
		return code, err, nil
	}
}

// dropSession forgets a session and tears it down, so the next command for this
// controller starts a new shell.
func (a *Agent) dropSession(fp string, sess *server.ShellSession) {
	a.sessMu.Lock()
	if a.sessions[fp] == sess {
		delete(a.sessions, fp)
	}
	a.sessMu.Unlock()
	sess.Close()
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
	case protocol.KindADBPair:
		if msg.PairPort < 1 || msg.PairPort > 65535 || len(msg.PairCode) != 6 || strings.TrimFunc(msg.PairCode, isDigit) != "" {
			return protocol.Message{Kind: protocol.KindError, Reason: "invalid pairing port or six-digit code"}
		}
		_, _, err := a.runADBPair(fmt.Sprintf("adb-pair %d %s", msg.PairPort, msg.PairCode), io.Discard)
		if err != nil {
			return protocol.Message{Kind: protocol.KindError, Reason: strings.ReplaceAll(err.Error(), msg.PairCode, "[redacted]")}
		}
		return protocol.Message{Kind: protocol.KindADBPair}

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
			a.notifyTrustChanged(msg.FP, "", "revoked")
		}
		return resp

	case protocol.KindTimeoutSet:
		// A front-end that pushes approvals somewhere a human reads slowly (a
		// phone) raises the wait; turning that off restores the default. Echo
		// back what actually took effect — the service clamps out-of-range
		// values instead of failing, and the front-end should display the truth.
		applied := a.console.SetTimeout(time.Duration(msg.TimeoutSec) * time.Second)
		return protocol.Message{Kind: protocol.KindTimeoutSet, TimeoutSec: int(applied / time.Second)}

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

	a.consoles.Add(1)
	defer a.consoles.Add(-1)

	var wmu sync.Mutex
	send := func(m protocol.Message) error {
		wmu.Lock()
		defer wmu.Unlock()
		return protocol.WriteMessage(conn, m)
	}

	// Push approval notifications asynchronously.
	ch, unsub := a.console.Subscribe()
	defer unsub()
	a.spawn(func() { pumpApprovalNotifs(ctx, ch, a.console, send) })

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

// Busy reports whether this agent is in the middle of work that restarting it
// would destroy: an open shell session (its cwd, its environment, its history),
// a background job whose output nobody has collected yet, or a live console
// session. It is the gate the auto-updater consults before swapping the binary
// under itself.
//
// A relay it cannot currently reach is deliberately not busy. That state can
// last for days on a laptop that is closed, and treating it as busy would mean
// the devices most in need of an unattended update are the ones that never get
// one.
func (a *Agent) Busy() bool {
	if a.workspacesBusy() {
		return true
	}
	a.sessMu.Lock()
	for _, sess := range a.sessions {
		if !sess.Closed() {
			a.sessMu.Unlock()
			return true
		}
	}
	a.sessMu.Unlock()
	// jobs is nil in unit tests that assemble an Agent without New.
	if a.jobs != nil && a.jobs.runningCount() > 0 {
		return true
	}
	return a.consoles.Load() > 0
}

// isPowerShell reports whether the resolved session shell is a PowerShell.
func isPowerShell(shell string) bool {
	s := strings.ToLower(shell)
	return strings.Contains(s, "powershell") || strings.Contains(s, "pwsh")
}

// DeviceID is the persistent routing identity. The fallback is only for embedded
// agents constructed without New (including legacy protocol tests).
func (a *Agent) DeviceID() string {
	if a.deviceID != "" {
		return a.deviceID
	}
	return a.opts.Name
}
