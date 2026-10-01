// Package console is the transport-neutral device console: a policy.Approver
// backed by a pending-approval queue plus rule/mode/log accessors. The agent
// uses it both as its approver and as the backend for remote (portal) console
// sessions. The CLI terminal and a remote portal feed decisions into the same
// queue — first answer wins.
package console

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"wanctl/internal/eventlog"
	"wanctl/internal/policy"
)

// Info is static device identity shown in the console.
type Info struct {
	Platform    string `json:"platform,omitempty"`
	ADBPair     bool   `json:"adb_pair,omitempty"`
	Device      string `json:"device"`
	Fingerprint string `json:"fingerprint"`
	Relay       string `json:"relay"`
}

// Pending is one approval awaiting a decision (JSON-safe view).
type Pending struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"`
	Cmd     string    `json:"cmd"`
	Path    string    `json:"path"`
	Cwd     string    `json:"cwd"`
	Peer    string    `json:"peer"`
	Created time.Time `json:"created"`
}

// PendingPairing is an unknown controller awaiting a trust decision (TOFU),
// surfaced to the portal so a human approves it on the web instead of at the
// device terminal.
type PendingPairing struct {
	FP      string    `json:"fp"`
	Name    string    `json:"name"`
	Label   string    `json:"label"` // controller's self-description (who/why)
	Created time.Time `json:"created"`
}

// TrustedController is one already-trusted controller, shown so the owner can
// revoke it.
type TrustedController struct {
	FP       string `json:"fp"`
	Name     string `json:"name"`
	Label    string `json:"label"`
	LastSeen string `json:"last_seen"`
}

// State is a full snapshot for a console front-end.
type State struct {
	Info            Info                `json:"info"`
	Mode            policy.Mode         `json:"mode"`
	Rules           []policy.Rule       `json:"rules"`
	Pending         []Pending           `json:"pending"`
	PendingPairings []PendingPairing    `json:"pending_pairings"`
	Trusted         []TrustedController `json:"trusted"`
	// ADB is the Android agent's adb elevation link, for the portal's ADB
	// card; absent on other platforms and from agents before v0.20.2.
	ADB *ADBLink `json:"adb,omitempty"`
}

// ADBLink is the adb elevation channel's state as the device last probed it:
// "off" (提权通道 switched off in the app), or one of elevate's Link states.
type ADBLink struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

type pending struct {
	view    Pending
	decided chan policy.Decision
}

type pendingPair struct {
	view    PendingPairing
	decided chan struct{} // closed once a verdict is recorded
	trust   bool          // valid after decided is closed
	expires time.Time     // TTL for retroactive approval (URL flow)
}

// pairTTL bounds how long an undecided pairing request lives in memory waiting
// for a user to click the URL the AI surfaced, and how long an approval waits
// for its controller to come back and collect it.
const pairTTL = 5 * time.Minute

// maxPendingPairs bounds how many unknown controllers can wait for the owner at
// once. Each is an entry held in memory and, when new, a notification to the
// owner, and a fresh key costs whoever dials nothing: without a bound, anyone
// able to reach the device could grow both without limit. Past it, new
// fingerprints are refused until requests are decided or expire. The ones
// already waiting are kept, so a flood cannot push a genuine request out of the
// owner's view.
const maxPendingPairs = 16

// pairNotifyWindow limits pairing notifications: within it, at most one per
// fingerprint and at most maxPendingPairs in all. The portal still shows every
// waiting request; only the push to the owner is held back.
const pairNotifyWindow = 30 * time.Minute

// ErrTooManyPairings is the answer to a new controller while maxPendingPairs
// requests are already waiting.
var ErrTooManyPairings = errors.New("too many pairing requests are already waiting for the device owner; try again later")

// DefaultTimeout is how long an approval waits for a front-end decision when
// nothing has raised it. It suits a human already looking at a screen.
const DefaultTimeout = 60 * time.Second

// MinTimeout and MaxTimeout bound what a front-end may ask for. A push
// notification to a phone — unlock, read, decide — routinely outlives
// DefaultTimeout, so the wait is adjustable; but it is also how long a
// controller's exec appears to hang, so it cannot be unbounded.
const (
	MinTimeout = 10 * time.Second
	MaxTimeout = 10 * time.Minute
)

// Service is the queue-backed console + approver.
type Service struct {
	engine  *policy.Engine
	log     *eventlog.Logger
	info    Info
	timeout time.Duration

	mu        sync.Mutex
	pend      map[string]*pending
	pairs     map[string]*pendingPair // keyed by controller fingerprint
	subs      map[chan struct{}]struct{}
	trustedFn func() []TrustedController // supplies the trusted-controller list (set by the agent)
	pendingFn func(Pending)              // best-effort observer; never runs on the approval path
	pairingFn func(PendingPairing)       // best-effort observer; called only for a new pairing entry
	notified  map[string]time.Time       // fingerprints recently reported to pairingFn (pairNotifyWindow)
}

// SetTimeout changes how long Ask and AskPair wait for a decision, and returns
// the value that actually took effect. Zero restores DefaultTimeout; anything
// else is clamped into [MinTimeout, MaxTimeout] rather than rejected, so a
// front-end cannot make a device wait forever and cannot accidentally make the
// window uselessly short either.
func (s *Service) SetTimeout(d time.Duration) time.Duration {
	if d == 0 {
		d = DefaultTimeout
	}
	if d < MinTimeout {
		d = MinTimeout
	}
	if d > MaxTimeout {
		d = MaxTimeout
	}
	s.mu.Lock()
	s.timeout = d
	s.mu.Unlock()
	return d
}

// Timeout reports the current approval wait.
func (s *Service) Timeout() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.timeout
}

// SetTrustedSource installs a callback returning the currently trusted
// controllers, so State can include them for the revoke UI.
func (s *Service) SetTrustedSource(fn func() []TrustedController) {
	s.mu.Lock()
	s.trustedFn = fn
	s.mu.Unlock()
}

// SetPendingHook observes newly queued approvals without participating in the
// decision. The hook always runs in its own goroutine.
func (s *Service) SetPendingHook(fn func(Pending)) {
	s.mu.Lock()
	s.pendingFn = fn
	s.mu.Unlock()
}

// SetPairingHook observes newly created pairing requests. Repeated dials that
// refresh the same pending fingerprint do not produce another event.
func (s *Service) SetPairingHook(fn func(PendingPairing)) {
	s.mu.Lock()
	s.pairingFn = fn
	s.mu.Unlock()
}

// New builds a console service bound to a policy engine and (optional) event log.
func New(engine *policy.Engine, log *eventlog.Logger, info Info) *Service {
	return &Service{
		engine: engine, log: log, info: info, timeout: DefaultTimeout,
		pend: map[string]*pending{}, pairs: map[string]*pendingPair{}, subs: map[chan struct{}]struct{}{},
		notified: map[string]time.Time{},
	}
}

// AskPair registers (or refreshes) a pending pair entry for an unknown
// controller and asks any subscribed front-end (the portal web console) to
// decide. Behavior matrix:
//
//   - already decided (e.g. user clicked the URL minutes ago)   → return p.trust
//   - subs > 0 and undecided  → block up to s.timeout for a live decision;
//     on timeout return false but LEAVE the entry intact (pairTTL) so the
//     user can still click the URL later.
//   - subs == 0 and undecided → return false immediately (fast-fail the dial)
//     but LEAVE the entry intact so the user can approve retroactively.
//
// This decouples controller-dial timing from portal-tab timing: the AI gets a
// reject + URL right away, the user clicks it whenever, the next dial finds the
// fp already trusted and goes through.
//
// An approval is collected once. The dial that receives it is the one the
// agent records in its trust store; keeping the verdict around afterwards
// would answer "trusted" to that key again later — after the owner revoked it,
// with nobody asked.
//
// A new fingerprint while maxPendingPairs requests are already waiting gets
// ErrTooManyPairings and no entry.
func (s *Service) AskPair(fp, name, label string) (bool, error) {
	return s.askPair(fp, name, label, true)
}

// AskPairNonBlocking publishes the same owner approval request but immediately
// returns its current decision. URL-only clients must receive the pairing link
// before their bounded task deadline, even while a portal console is watching.
func (s *Service) AskPairNonBlocking(fp, name, label string) (bool, error) {
	return s.askPair(fp, name, label, false)
}

func (s *Service) askPair(fp, name, label string, waitForDecision bool) (bool, error) {
	s.mu.Lock()
	s.pruneExpiredPairsLocked()
	p := s.pairs[fp]
	created := false
	if p != nil {
		// Already decided? Return its verdict, which this collects.
		select {
		case <-p.decided:
			trust := p.trust
			delete(s.pairs, fp)
			s.mu.Unlock()
			return trust, nil
		default:
		}
		// Same fp dialing again — refresh metadata + TTL, reuse the entry.
		if name != "" {
			p.view.Name = name
		}
		if label != "" {
			p.view.Label = label
		}
		p.expires = time.Now().Add(pairTTL)
	} else {
		if s.undecidedPairsLocked() >= maxPendingPairs {
			s.mu.Unlock()
			return false, ErrTooManyPairings
		}
		created = true
		p = &pendingPair{
			view:    PendingPairing{FP: fp, Name: name, Label: label, Created: time.Now()},
			decided: make(chan struct{}),
			expires: time.Now().Add(pairTTL),
		}
		s.pairs[fp] = p
	}
	hasFrontend := waitForDecision && len(s.subs) > 0
	pairingFn := s.pairingFn
	report := created && pairingFn != nil && s.claimPairNotifyLocked(fp)
	pairingView := p.view
	decided := p.decided
	wait := s.timeout
	s.mu.Unlock()
	s.notify()
	if report {
		go pairingFn(pairingView)
	}

	if !hasFrontend {
		// Headless or no portal tab attending; let the controller fail fast and
		// surface a URL to the user. The entry persists (pairTTL) for offline
		// approval.
		return false, nil
	}
	select {
	case <-decided:
		s.mu.Lock()
		trust := p.trust
		if s.pairs[fp] == p {
			delete(s.pairs, fp) // collected, see AskPair
		}
		s.mu.Unlock()
		return trust, nil
	case <-time.After(wait):
		// Front-end was attending but didn't decide in time. Entry persists for
		// retroactive approval; this dial reports a reject + URL.
		return false, nil
	}
}

// undecidedPairsLocked counts the requests still waiting for the owner. Caller
// holds s.mu.
func (s *Service) undecidedPairsLocked() int {
	n := 0
	for _, p := range s.pairs {
		select {
		case <-p.decided:
		default:
			n++
		}
	}
	return n
}

// claimPairNotifyLocked reports whether a new request from fp may notify the
// owner, and records it if so. Caller holds s.mu. The record is pruned to the
// window on every call, and never holds more than maxPendingPairs entries.
func (s *Service) claimPairNotifyLocked(fp string) bool {
	now := time.Now()
	for k, at := range s.notified {
		if now.Sub(at) >= pairNotifyWindow {
			delete(s.notified, k)
		}
	}
	if _, recent := s.notified[fp]; recent || len(s.notified) >= maxPendingPairs {
		return false
	}
	s.notified[fp] = now
	return true
}

// DecidePair delivers a trust verdict for a pending pairing. Returns false if
// the fingerprint is unknown or already decided. On approval the entry is kept
// (so the next AskPair from the same fp returns true immediately and the agent
// can AddLabeled to known_clients). On denial the entry is dropped so a future
// retry produces a fresh URL the user can act on differently.
func (s *Service) DecidePair(fp string, trust bool) bool {
	s.mu.Lock()
	// Prune first: a verdict can arrive from a page that has been open longer
	// than the request lived. pairTTL is the window for answering, so past it
	// the answer is refused rather than silently granting trust for a request
	// the user can no longer see in context.
	s.pruneExpiredPairsLocked()
	p := s.pairs[fp]
	if p == nil {
		s.mu.Unlock()
		return false
	}
	select {
	case <-p.decided:
		s.mu.Unlock()
		return false // already decided
	default:
	}
	p.trust = trust
	close(p.decided)
	if !trust {
		delete(s.pairs, fp)
	} else {
		// The approval waits pairTTL for its controller to collect it, and
		// no longer: an approval left lying around would admit that key
		// whenever it next showed up.
		p.expires = time.Now().Add(pairTTL)
	}
	s.mu.Unlock()
	s.notify()
	return true
}

// pruneExpiredPairsLocked drops pair entries past their TTL: requests nobody
// answered, and approvals nobody collected. Caller holds s.mu.
func (s *Service) pruneExpiredPairsLocked() {
	now := time.Now()
	for fp, p := range s.pairs {
		if now.After(p.expires) {
			delete(s.pairs, fp)
		}
	}
}

// Ask implements policy.Approver: enqueue and block until a front-end decides
// or the timeout elapses (then deny).
//
// If no console front-end is subscribed (headless: no portal session, no TTY
// prompt), we deny immediately rather than blocking until timeout. Operators
// pre-load allow-rules so that legitimate operations are never enqueued at all.
func (s *Service) Ask(req policy.Request) policy.Decision {
	s.mu.Lock()
	hasFrontend := len(s.subs) > 0
	wait := s.timeout
	s.mu.Unlock()
	if !hasFrontend {
		// No console front-end is listening (headless: no portal session, no TTY
		// prompt). Deny by default rather than blocking until timeout; operators
		// pre-load rules to allow specific operations.
		return policy.Decision{Allow: false}
	}
	id := newID()
	p := &pending{
		view: Pending{
			// CommandLabel, not the raw command: a `-script` run arrives as a
			// 24 KB base64 blob, and a front-end that draws it asks a human to
			// approve something nobody can read. The label is the same token
			// the remembered rule will carry, so card and rule agree.
			ID: id, Kind: string(req.Kind), Cmd: policy.CommandLabel(req.Cmd), Path: req.Path,
			Cwd: req.Cwd, Peer: req.Peer, Created: time.Now(),
		},
		decided: make(chan policy.Decision, 1),
	}
	s.mu.Lock()
	s.pend[id] = p
	pendingFn := s.pendingFn
	s.mu.Unlock()
	s.notify()
	if pendingFn != nil {
		go pendingFn(p.view)
	}
	defer func() {
		s.mu.Lock()
		delete(s.pend, id)
		s.mu.Unlock()
		s.notify()
	}()
	select {
	case d := <-p.decided:
		return d
	case <-time.After(wait):
		return policy.Decision{Allow: false}
	}
}

// State returns a snapshot for a front-end.
//
// It prunes first. Until it did, pairTTL only ever elapsed for a controller that
// dialled again — askPair was the sole pruner — so a request nobody retried
// stayed in this snapshot forever. The portal kept drawing a card the device
// would no longer honour, and pressing it read as a dead button (issue #79).
func (s *Service) State() State {
	s.mu.Lock()
	s.pruneExpiredPairsLocked()
	pend := make([]Pending, 0, len(s.pend))
	for _, p := range s.pend {
		pend = append(pend, p.view)
	}
	pairs := make([]PendingPairing, 0, len(s.pairs))
	for _, p := range s.pairs {
		// Hide already-decided entries from the front-end (they linger only so
		// the next AskPair can drain the verdict).
		select {
		case <-p.decided:
		default:
			pairs = append(pairs, p.view)
		}
	}
	trustedFn := s.trustedFn
	s.mu.Unlock()
	var trusted []TrustedController
	if trustedFn != nil {
		trusted = trustedFn()
	}
	return State{Info: s.info, Mode: s.engine.Mode(), Rules: s.engine.List(), Pending: pend, PendingPairings: pairs, Trusted: trusted}
}

// Decide delivers a verdict to a pending request. Returns false if unknown.
// Verdict maps y/a/g/n exactly like the console approver. The agent gate is the
// single source of truth for remembering rules, so we never Add here.
func (s *Service) Decide(id, verdict string) bool {
	s.mu.Lock()
	p := s.pend[id]
	s.mu.Unlock()
	if p == nil {
		return false
	}
	var d policy.Decision
	switch verdict {
	case "y", "yes":
		d = policy.Decision{Allow: true}
	case "a":
		if p.view.Kind == string(policy.KindLogs) {
			d = policy.Decision{Allow: true}
		} else {
			d = policy.Decision{Allow: true, Remember: true, Scope: policy.ScopeDir}
		}
	case "g":
		d = policy.Decision{Allow: true, Remember: true, Scope: policy.ScopeGlobal}
	default:
		d = policy.Decision{Allow: false}
	}
	select {
	case p.decided <- d:
	default:
	}
	return true
}

// AddRule / RemoveRule / SetMode proxy the engine and notify subscribers.
func (s *Service) AddRule(r policy.Rule) error { err := s.engine.Add(r); s.notify(); return err }
func (s *Service) RemoveRule(i int) error      { err := s.engine.Remove(i); s.notify(); return err }
func (s *Service) SetMode(m policy.Mode)       { s.engine.SetMode(m); s.notify() }

// Logs returns the last `limit` events (0 -> 100).
func (s *Service) Logs(limit int) []eventlog.Event {
	if s.log == nil {
		return nil
	}
	if limit <= 0 {
		limit = 100
	}
	ev, _ := s.log.Read(eventlog.Filter{Limit: limit})
	return ev
}

// Subscribe returns a channel pinged on any state change, plus a cancel func.
func (s *Service) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}
}

// Notify pushes a state-changed signal to subscribers (used after the agent
// mutates trust outside the console, e.g. revoking a controller).
func (s *Service) Notify() { s.notify() }

func (s *Service) notify() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func newID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
