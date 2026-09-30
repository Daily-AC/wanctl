package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"wanctl/internal/console"
	"wanctl/internal/protocol"
)

// fakePhoneSession stands in for one device's console session: a target, the
// phone, or both.
type fakePhoneSession struct {
	mu        sync.Mutex
	states    chan console.State
	replyCh   chan protocol.Message
	pushes    []protocol.ApprovalCard
	pushErr   error
	decisions []string // "id verdict approver"
	found     bool
	grants    []string // "kind|pattern|fp|ttl|approver"
	pairs     []string
	pairErr   error
	timeouts  []int
}

func newFakePhoneSession() *fakePhoneSession {
	return &fakePhoneSession{states: make(chan console.State, 8), replyCh: make(chan protocol.Message, 8), found: true}
}

func (f *fakePhoneSession) subscribe() (<-chan console.State, func()) { return f.states, func() {} }
func (f *fakePhoneSession) decide(id, verdict, approver string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decisions = append(f.decisions, id+" "+verdict+" "+approver)
	return nil
}
func (f *fakePhoneSession) decideFound(id, verdict, approver string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decisions = append(f.decisions, id+" "+verdict+" "+approver)
	return f.found, nil
}
func (f *fakePhoneSession) pairDecide(fp, verdict string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pairs = append(f.pairs, fp+" "+verdict)
	return f.pairErr
}
func (f *fakePhoneSession) setApprovalTimeout(sec int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.timeouts = append(f.timeouts, sec)
	return sec, nil
}
func (f *fakePhoneSession) grantOnce(kind, pattern, fp string, ttl time.Duration, approver string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.grants = append(f.grants, strings.Join([]string{kind, pattern, fp, ttl.String(), approver}, "|"))
	return nil
}
func (f *fakePhoneSession) approvalPush(card protocol.ApprovalCard, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pushErr != nil {
		return f.pushErr
	}
	f.pushes = append(f.pushes, card)
	return nil
}
func (f *fakePhoneSession) replies() (<-chan protocol.Message, func()) { return f.replyCh, func() {} }
func (f *fakePhoneSession) state() (console.State, error)              { return console.State{}, nil }

func (f *fakePhoneSession) lastPush(t *testing.T) protocol.ApprovalCard {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pushes) == 0 {
		t.Fatal("nothing pushed to the phone")
	}
	return f.pushes[len(f.pushes)-1]
}

func (f *fakePhoneSession) snapshot() (pushes []protocol.ApprovalCard, decisions, grants, pairs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]protocol.ApprovalCard(nil), f.pushes...), append([]string(nil), f.decisions...),
		append([]string(nil), f.grants...), append([]string(nil), f.pairs...)
}

type phoneFixture struct {
	sup     *phoneSupervisor
	phone   *fakePhoneSession
	target  *fakePhoneSession
	clock   time.Time
	clockMu sync.Mutex
	dropped []string
	dropMu  sync.Mutex
	closed  int // link sessions closed, guarded by dropMu

	mu       sync.Mutex
	phones   map[string]string // ns -> designated device
	devices  []phoneDeviceRow
	failDial map[string]bool
}

func newPhoneFixture(t *testing.T) *phoneFixture {
	t.Helper()
	f := &phoneFixture{
		phone:  newFakePhoneSession(),
		target: newFakePhoneSession(),
		clock:  time.Date(2026, 9, 30, 13, 0, 0, 0, time.UTC),
		phones: map[string]string{"alice": "phone-1"},
		devices: []phoneDeviceRow{
			{Name: "phone-1", DisplayName: "pgbm10", Owner: "alice", Online: true},
			{Name: "mac-1", DisplayName: "mac", Owner: "alice", Online: true},
			{Name: "bobs", DisplayName: "bobs", Owner: "bob", Shared: true, Online: true},
		},
		failDial: map[string]bool{},
	}
	sup := &phoneSupervisor{
		sessionFor: func(_ context.Context, ns, device string) (phoneSession, error) {
			f.mu.Lock()
			fail := f.failDial[device]
			f.mu.Unlock()
			if fail {
				return nil, errors.New("dial failed")
			}
			switch device {
			case "phone-1":
				return f.phone, nil
			case "mac-1":
				return f.target, nil
			}
			return nil, errors.New("no such device " + device)
		},
		linkFor: func(ctx context.Context, ns, device string) (phoneSession, func(), error) {
			sess, err := f.sup.sessionFor(ctx, ns, device)
			if err != nil {
				return nil, nil, err
			}
			return sess, func() { f.dropMu.Lock(); f.closed++; f.dropMu.Unlock() }, nil
		},
		dropConn: func(ns, device string) {
			f.dropMu.Lock()
			f.dropped = append(f.dropped, ns+"/"+device)
			f.dropMu.Unlock()
		},
		adminReq:       f.adminReq,
		logf:           t.Logf,
		now:            func() time.Time { f.clockMu.Lock(); defer f.clockMu.Unlock(); return f.clock },
		reconcileEvery: time.Hour, // tests reconcile by hand
		retryMin:       time.Millisecond,
		retryMax:       time.Millisecond,
		waitRetryFn:    waitContext,
		wake:           make(chan struct{}, 1),
		spaces:         map[string]*phoneSpace{},
		cards:          map[string]*phoneCard{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	sup.ctx, sup.cancel = ctx, cancel
	t.Cleanup(func() { cancel(); sup.stopAll(); sup.wg.Wait() })
	f.sup = sup
	return f
}

func (f *phoneFixture) advance(d time.Duration) {
	f.clockMu.Lock()
	f.clock = f.clock.Add(d)
	f.clockMu.Unlock()
}

func (f *phoneFixture) adminReq(method, path string, query url.Values, _ any) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body any
	switch path {
	case "/admin/approval-phone":
		var phones []map[string]string
		for ns, d := range f.phones {
			phones = append(phones, map[string]string{"namespace": ns, "device": d})
		}
		body = map[string]any{"phones": phones}
	case "/admin/devices":
		body = map[string]any{"devices": f.devices}
	default:
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("no"))}, nil
	}
	b, _ := json.Marshal(body)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b))}, nil
}

func phoneWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (f *phoneFixture) upAndWatching(t *testing.T) {
	t.Helper()
	if err := f.sup.reconcile(); err != nil {
		t.Fatal(err)
	}
	if dev, online := f.sup.status("alice"); dev != "phone-1" || !online {
		t.Fatalf("status = %q %v, want phone-1 online", dev, online)
	}
	// The resident watch raised the wait on the target before subscribing.
	phoneWaitFor(t, "target watch", func() bool {
		f.target.mu.Lock()
		defer f.target.mu.Unlock()
		return len(f.target.timeouts) > 0 && f.target.timeouts[0] == 180
	})
}

func controllerFP() string { return "sha256:" + strings.Repeat("cd", 32) }

func pendingState(id, cmd string) console.State {
	return console.State{
		Pending: []console.Pending{{ID: id, Kind: "exec", Cmd: cmd, Peer: controllerFP(), Created: time.Now()}},
		Trusted: []console.TrustedController{{FP: controllerFP(), Name: "zyl-mac"}},
	}
}

// Only owned, online devices are watched: a device shared with the namespace
// belongs to someone else's phone.
func TestPhoneWatchesOwnedOnlineDevicesOnly(t *testing.T) {
	f := newPhoneFixture(t)
	f.upAndWatching(t)
	f.sup.mu.Lock()
	defer f.sup.mu.Unlock()
	sp := f.sup.spaces["alice"]
	if sp.phoneName != "pgbm10" {
		t.Fatalf("phone name = %q", sp.phoneName)
	}
	if len(sp.targets) != 2 || sp.targets["phone-1"] == nil || sp.targets["mac-1"] == nil {
		t.Fatalf("targets = %v, want phone-1 and mac-1", sp.targets)
	}
}

func TestPhoneApprovalReachesTheDevice(t *testing.T) {
	f := newPhoneFixture(t)
	f.upAndWatching(t)
	f.target.states <- pendingState("p1", "Get-Service wanctl")
	phoneWaitFor(t, "push", func() bool { p, _, _, _ := f.phone.snapshot(); return len(p) == 1 })
	card := f.phone.lastPush(t)
	if card.State != protocol.ApprovalPending || card.Device != "mac" || card.Cmd != "Get-Service wanctl" ||
		card.Peer != "zyl-mac" || card.PeerFP != controllerFP() || card.Kind != "exec" || len(card.ID) != 32 {
		t.Fatalf("card = %+v", card)
	}
	if got := card.Expires.Sub(f.clock); got != 180*time.Second {
		t.Fatalf("card expires in %s, want the applied 180s wait", got)
	}

	f.phone.replyCh <- protocol.Message{Kind: protocol.KindApprovalReply, ApprovalID: card.ID, Verdict: "y"}
	phoneWaitFor(t, "decision", func() bool { _, d, _, _ := f.target.snapshot(); return len(d) == 1 })
	if _, d, _, _ := f.target.snapshot(); d[0] != "p1 y phone:pgbm10" {
		t.Fatalf("decision = %q", d[0])
	}
	phoneWaitFor(t, "confirmation", func() bool { return f.phone.lastPush(t).State == protocol.ApprovalDone })
	if got := f.phone.lastPush(t); got.ID != card.ID || got.Result != protocol.ResultAllowed {
		t.Fatalf("confirmation = %+v", got)
	}

	// The same answer again (the phone resends after a reconnect) repeats the
	// outcome and never reaches the device a second time.
	f.phone.replyCh <- protocol.Message{Kind: protocol.KindApprovalReply, ApprovalID: card.ID, Verdict: "y"}
	phoneWaitFor(t, "repeat", func() bool { p, _, _, _ := f.phone.snapshot(); return len(p) == 3 })
	if _, d, _, _ := f.target.snapshot(); len(d) != 1 {
		t.Fatalf("decisions after a resend = %v", d)
	}
	// The request leaving the device afterwards changes nothing.
	f.target.states <- console.State{}
	time.Sleep(20 * time.Millisecond)
	if p, _, _, _ := f.phone.snapshot(); len(p) != 3 {
		t.Fatalf("pushes after settle = %d", len(p))
	}
}

// ADR 0015 decision 2: an approval after the wait turns into a one-shot grant
// for the same controller and command.
func TestPhoneLateApprovalGrantsOnce(t *testing.T) {
	f := newPhoneFixture(t)
	f.upAndWatching(t)
	f.target.states <- pendingState("p1", "Get-Service wanctl")
	phoneWaitFor(t, "push", func() bool { p, _, _, _ := f.phone.snapshot(); return len(p) == 1 })
	id := f.phone.lastPush(t).ID

	f.advance(181 * time.Second)
	f.target.states <- console.State{}
	phoneWaitFor(t, "expired card", func() bool { return f.phone.lastPush(t).State == protocol.ApprovalExpired })

	f.phone.replyCh <- protocol.Message{Kind: protocol.KindApprovalReply, ApprovalID: id, Verdict: "y"}
	phoneWaitFor(t, "grant", func() bool { _, _, g, _ := f.target.snapshot(); return len(g) == 1 })
	_, d, g, _ := f.target.snapshot()
	if len(d) != 0 {
		t.Fatalf("an expired request was decided directly: %v", d)
	}
	if want := "exec|Get-Service wanctl|" + controllerFP() + "|30m0s|phone:pgbm10"; g[0] != want {
		t.Fatalf("grant = %q, want %q", g[0], want)
	}
	phoneWaitFor(t, "granted", func() bool { return f.phone.lastPush(t).Result == protocol.ResultGranted })
}

// Tapping allow in the seconds between expiry and the portal noticing: the
// device says not-found, and the tap becomes the late grant.
func TestPhoneApprovalRacingExpiryBecomesGrant(t *testing.T) {
	f := newPhoneFixture(t)
	f.upAndWatching(t)
	f.target.found = false
	f.target.states <- pendingState("p1", "ls")
	phoneWaitFor(t, "push", func() bool { p, _, _, _ := f.phone.snapshot(); return len(p) == 1 })
	f.phone.replyCh <- protocol.Message{Kind: protocol.KindApprovalReply, ApprovalID: f.phone.lastPush(t).ID, Verdict: "y"}
	phoneWaitFor(t, "grant", func() bool { _, _, g, _ := f.target.snapshot(); return len(g) == 1 })
}

func TestPhoneRequestAnsweredElsewhereIsHandled(t *testing.T) {
	f := newPhoneFixture(t)
	f.upAndWatching(t)
	f.target.states <- pendingState("p1", "ls")
	phoneWaitFor(t, "push", func() bool { p, _, _, _ := f.phone.snapshot(); return len(p) == 1 })
	id := f.phone.lastPush(t).ID
	f.advance(20 * time.Second) // well inside the wait: someone answered it in the portal
	f.target.states <- console.State{}
	phoneWaitFor(t, "handled", func() bool { return f.phone.lastPush(t).Result == protocol.ResultHandled })

	// A tap that arrives afterwards cannot reach the device.
	f.phone.replyCh <- protocol.Message{Kind: protocol.KindApprovalReply, ApprovalID: id, Verdict: "y"}
	time.Sleep(20 * time.Millisecond)
	if _, d, g, _ := f.target.snapshot(); len(d) != 0 || len(g) != 0 {
		t.Fatalf("decisions %v grants %v after the request was handled", d, g)
	}
}

// Decisions are accepted only for ids this portal issued, and only from the
// namespace's current phone.
func TestPhoneRejectsForeignAndStaleDecisions(t *testing.T) {
	f := newPhoneFixture(t)
	f.upAndWatching(t)
	f.phone.replyCh <- protocol.Message{Kind: protocol.KindApprovalReply, ApprovalID: "made-up", Verdict: "y"}
	phoneWaitFor(t, "gone", func() bool { p, _, _, _ := f.phone.snapshot(); return len(p) == 1 })
	if got := f.phone.lastPush(t); got.ID != "made-up" || got.Result != protocol.ResultGone {
		t.Fatalf("answer to a made-up id = %+v", got)
	}

	f.target.states <- pendingState("p1", "ls")
	phoneWaitFor(t, "push", func() bool { p, _, _, _ := f.phone.snapshot(); return len(p) == 2 })
	id := f.phone.lastPush(t).ID
	// The same id arriving over another device's session (not the phone) is
	// not a decision.
	f.sup.handleReply("alice", "mac-1", protocol.Message{Kind: protocol.KindApprovalReply, ApprovalID: id, Verdict: "y"})
	// Nor from the phone after it stopped being the phone.
	f.mu.Lock()
	f.phones = map[string]string{}
	f.mu.Unlock()
	if err := f.sup.reconcile(); err != nil {
		t.Fatal(err)
	}
	f.sup.handleReply("alice", "phone-1", protocol.Message{Kind: protocol.KindApprovalReply, ApprovalID: id, Verdict: "y"})
	if _, d, g, _ := f.target.snapshot(); len(d) != 0 || len(g) != 0 {
		t.Fatalf("decisions %v grants %v from a phone that may not decide", d, g)
	}
	if dev, _ := f.sup.status("alice"); dev != "" {
		t.Fatalf("withdrawn phone still watched: %q", dev)
	}
}

// A push the phone cannot take refuses that request at once and stops
// watching, so the next request is refused on the device instead of hanging
// for three minutes.
func TestPhoneUnreachableRefusesAndStopsWatching(t *testing.T) {
	f := newPhoneFixture(t)
	f.upAndWatching(t)
	f.phone.mu.Lock()
	f.phone.pushErr = errors.New("device did not respond within 8s (re-dialing)")
	f.phone.mu.Unlock()
	f.target.states <- pendingState("p1", "ls")
	phoneWaitFor(t, "refusal", func() bool { _, d, _, _ := f.target.snapshot(); return len(d) == 1 })
	if _, d, _, _ := f.target.snapshot(); d[0] != "p1 n system:approval-phone-unreachable" {
		t.Fatalf("decision = %q", d[0])
	}
	phoneWaitFor(t, "teardown", func() bool {
		_, online := f.sup.status("alice")
		f.dropMu.Lock()
		defer f.dropMu.Unlock()
		return !online && len(f.dropped) == 2
	})
	// The watch restored the device's own default wait on the way out.
	f.target.mu.Lock()
	last := f.target.timeouts[len(f.target.timeouts)-1]
	f.target.mu.Unlock()
	if last != 0 {
		t.Fatalf("last timeout set = %d, want 0 (restore)", last)
	}
}

func TestPhoneOfflineInRegistryWatchesNothing(t *testing.T) {
	f := newPhoneFixture(t)
	f.upAndWatching(t)
	f.mu.Lock()
	f.devices[0].Online = false
	f.mu.Unlock()
	if err := f.sup.reconcile(); err != nil {
		t.Fatal(err)
	}
	if _, online := f.sup.status("alice"); online {
		t.Fatal("phone still online")
	}
	f.sup.mu.Lock()
	n := len(f.sup.spaces["alice"].targets)
	f.sup.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d devices still watched with the phone offline", n)
	}
	// Back online: watched again.
	f.mu.Lock()
	f.devices[0].Online = true
	f.mu.Unlock()
	if err := f.sup.reconcile(); err != nil {
		t.Fatal(err)
	}
	if _, online := f.sup.status("alice"); !online {
		t.Fatal("phone not back")
	}
}

func TestPhonePairingRequest(t *testing.T) {
	f := newPhoneFixture(t)
	f.upAndWatching(t)
	fp := controllerFP()
	f.target.states <- console.State{PendingPairings: []console.PendingPairing{{FP: fp, Name: "new-laptop", Label: "Claude Code", Created: time.Now()}}}
	phoneWaitFor(t, "push", func() bool { p, _, _, _ := f.phone.snapshot(); return len(p) == 1 })
	card := f.phone.lastPush(t)
	if card.Kind != "pair" || card.PeerFP != fp || card.Peer != "new-laptop（Claude Code）" {
		t.Fatalf("pairing card = %+v", card)
	}
	if got := card.Expires.Sub(f.clock); got != 5*time.Minute {
		t.Fatalf("pairing card expires in %s, want the device's 5-minute window", got)
	}
	f.phone.replyCh <- protocol.Message{Kind: protocol.KindApprovalReply, ApprovalID: card.ID, Verdict: "y"}
	phoneWaitFor(t, "pair decision", func() bool { _, _, _, p := f.target.snapshot(); return len(p) == 1 })
	if _, _, _, p := f.target.snapshot(); p[0] != fp+" y" {
		t.Fatalf("pair decision = %q", p[0])
	}
	phoneWaitFor(t, "allowed", func() bool { return f.phone.lastPush(t).Result == protocol.ResultAllowed })
}

func TestPhoneLatePairingIsGone(t *testing.T) {
	f := newPhoneFixture(t)
	f.upAndWatching(t)
	fp := controllerFP()
	f.target.states <- console.State{PendingPairings: []console.PendingPairing{{FP: fp, Name: "x"}}}
	phoneWaitFor(t, "push", func() bool { p, _, _, _ := f.phone.snapshot(); return len(p) == 1 })
	id := f.phone.lastPush(t).ID
	f.advance(5 * time.Minute)
	f.target.states <- console.State{}
	phoneWaitFor(t, "expired", func() bool { return f.phone.lastPush(t).State == protocol.ApprovalExpired })
	f.phone.replyCh <- protocol.Message{Kind: protocol.KindApprovalReply, ApprovalID: id, Verdict: "y"}
	phoneWaitFor(t, "gone", func() bool { return f.phone.lastPush(t).Result == protocol.ResultGone })
	if _, _, g, p := f.target.snapshot(); len(g) != 0 || len(p) != 0 {
		t.Fatalf("a stale pairing reached the device: grants %v pairs %v", g, p)
	}
}

func TestPhoneCardsArePruned(t *testing.T) {
	f := newPhoneFixture(t)
	f.upAndWatching(t)
	for i := 0; i < phoneMaxCards+5; i++ {
		f.sup.newCard("alice", "mac-1", protocol.ApprovalCard{Kind: "exec"}, time.Minute)
		f.advance(time.Millisecond)
	}
	f.sup.pruneCards()
	f.sup.mu.Lock()
	n := len(f.sup.cards)
	f.sup.mu.Unlock()
	if n != phoneMaxCards {
		t.Fatalf("cards = %d, want %d", n, phoneMaxCards)
	}
	f.advance(phoneCardRetention)
	f.sup.pruneCards()
	f.sup.mu.Lock()
	n = len(f.sup.cards)
	f.sup.mu.Unlock()
	if n != 0 {
		t.Fatalf("cards past retention = %d", n)
	}
}

// The link owns its session. The pooled connection every portal page shares
// is closed whenever a page's status query times out, which on a real phone
// tore the watch down every few seconds (S13, 2026-09-30); a card must still
// reach the phone when the pool cannot.
func TestPhoneLinkDoesNotUseThePool(t *testing.T) {
	f := newPhoneFixture(t)
	f.upAndWatching(t)
	f.mu.Lock()
	f.failDial["phone-1"] = true // the pool can no longer reach the phone
	f.mu.Unlock()
	f.target.states <- pendingState("p1", "echo s13")
	phoneWaitFor(t, "push over the link", func() bool { p, _, _, _ := f.phone.snapshot(); return len(p) == 1 })
	if _, online := f.sup.status("alice"); !online {
		t.Fatal("phone went offline although its link is up")
	}

	f.sup.teardown("alice", false)
	f.dropMu.Lock()
	defer f.dropMu.Unlock()
	if f.closed != 1 {
		t.Fatalf("link sessions closed = %d, want 1", f.closed)
	}
}
