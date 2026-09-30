package portal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"

	"wanctl/internal/console"
	"wanctl/internal/protocol"
)

// The approval phone (ADR 0015). While a namespace's designated phone is
// online, the portal keeps a resident watch on every online device the
// namespace owns, raises their approval wait, and pushes each pending approval
// and pairing to the phone. The owner's decision comes back over the phone's
// console session and is forwarded to the device it was for; one that arrives
// after the wait ran out becomes a one-shot grant on that device.
const (
	phoneApprovalWait   = 180 * time.Second
	phonePairingWindow  = 5 * time.Minute // what a device keeps an unanswered pairing for (console.pairTTL)
	phonePushTimeout    = 8 * time.Second
	phoneCardRetention  = 24 * time.Hour
	phoneMaxCards       = 256 // per namespace; the oldest go first
	phoneReconcileEvery = 30 * time.Second
	// A request that disappears this close to the end of its wait timed out;
	// earlier than that, somebody answered it somewhere else.
	phoneExpirySlack = 5 * time.Second
)

// phoneSession is what the approval phone workflow needs from a console
// session. *deviceConn satisfies it.
type phoneSession interface {
	deviceSession
	decideFound(id, verdict, approver string) (bool, error)
	grantOnce(kind, pattern, fp string, ttl time.Duration, approver string) error
	approvalPush(card protocol.ApprovalCard, timeout time.Duration) error
	replies() (<-chan protocol.Message, func())
	state() (console.State, error)
}

type phoneSupervisor struct {
	sessionFor func(ctx context.Context, ns, device string) (phoneSession, error)
	// linkFor dials a session nobody else uses, for the phone's link; the
	// func closes it.
	linkFor  func(ctx context.Context, ns, device string) (phoneSession, func(), error)
	dropConn func(ns, device string)
	adminReq adminRequestFunc
	logf     func(string, ...any)
	now      func() time.Time

	reconcileEvery time.Duration
	retryMin       time.Duration
	retryMax       time.Duration
	waitRetryFn    func(context.Context, time.Duration) bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	wake   chan struct{}

	mu     sync.Mutex
	spaces map[string]*phoneSpace // by namespace
	cards  map[string]*phoneCard  // by card id
}

// phoneSpace is one namespace with a designated phone.
type phoneSpace struct {
	ns        string
	phone     string // route name, the key the portal dials it by
	phoneName string // display name, for approver=phone:<name>
	online    bool
	session   phoneSession // the link's own console session, set while online
	link      func()       // stops the reply listener and closes session
	targets   map[string]*phoneTarget
	names     map[string]string // route name -> display name, for the cards
}

type phoneTarget struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// phoneCard is the portal's record of one card: what it was for, which phone
// it went to, and how it ended. The card id is the only thing the phone can
// name in a decision, so this record is what makes a decision valid.
type phoneCard struct {
	card      protocol.ApprovalCard // card.Device is the display name
	ns        string
	device    string // route name of the target device
	phone     string // the phone it was pushed to; a decision must come from it
	pendingID string // approval on the target device, or
	pairFP    string // pairing on the target device
	seenAt    time.Time
	answered  bool
}

func newPhoneSupervisor(s *Server) *phoneSupervisor {
	return &phoneSupervisor{
		sessionFor: func(ctx context.Context, ns, device string) (phoneSession, error) {
			return s.deviceConnFor(ctx, ns, device)
		},
		linkFor: func(ctx context.Context, ns, device string) (phoneSession, func(), error) {
			d, err := s.dialDeviceConn(ctx, ns, device)
			if err != nil {
				return nil, nil, err
			}
			return d, d.close, nil
		},
		dropConn:       s.dropConn,
		adminReq:       s.adminReq,
		logf:           log.Printf,
		now:            time.Now,
		reconcileEvery: phoneReconcileEvery,
		retryMin:       larkRetryMin,
		retryMax:       larkRetryMax,
		waitRetryFn:    waitContext,
		wake:           make(chan struct{}, 1),
		spaces:         make(map[string]*phoneSpace),
		cards:          make(map[string]*phoneCard),
	}
}

func (p *phoneSupervisor) start(parent context.Context) {
	p.ctx, p.cancel = context.WithCancel(parent)
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ticker := time.NewTicker(p.reconcileEvery)
		defer ticker.Stop()
		for {
			if err := p.reconcile(); err != nil {
				p.logf("approval phone reconcile: %v", err)
			}
			select {
			case <-p.ctx.Done():
				p.stopAll()
				return
			case <-ticker.C:
			case <-p.wake:
			}
		}
	}()
}

func (p *phoneSupervisor) stop() {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
}

func (p *phoneSupervisor) triggerReconcile() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *phoneSupervisor) launch(fn func(context.Context)) {
	if p.ctx == nil || p.ctx.Err() != nil {
		return
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		fn(p.ctx)
	}()
}

type phoneDeviceRow struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Owner       string `json:"owner"`
	Shared      bool   `json:"shared"`
	Online      bool   `json:"online"`
}

func (p *phoneSupervisor) loadPhones() (map[string]string, error) {
	resp, err := p.adminReq(http.MethodGet, "/admin/approval-phone", nil, nil)
	if err != nil {
		return nil, fmt.Errorf("list approval phones: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, responseError("list approval phones", resp)
	}
	var out struct {
		Phones []struct {
			Namespace string `json:"namespace"`
			Device    string `json:"device"`
		} `json:"phones"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode approval phones: %w", err)
	}
	phones := make(map[string]string, len(out.Phones))
	for _, ph := range out.Phones {
		if ph.Namespace != "" && ph.Device != "" {
			phones[ph.Namespace] = ph.Device
		}
	}
	return phones, nil
}

// ownedDevices lists the devices ns owns, with the relay's live registry view
// of whether each is online. A device shared with ns belongs to someone else's
// phone, not this one.
func (p *phoneSupervisor) ownedDevices(ns string) ([]phoneDeviceRow, error) {
	resp, err := p.adminReq(http.MethodGet, "/admin/devices", url.Values{"namespace": {ns}}, nil)
	if err != nil {
		return nil, fmt.Errorf("list devices for %q: %w", ns, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, responseError("list devices for "+ns, resp)
	}
	var out struct {
		Devices []phoneDeviceRow `json:"devices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode devices for %q: %w", ns, err)
	}
	owned := out.Devices[:0]
	for _, d := range out.Devices {
		if !d.Shared && (d.Owner == "" || d.Owner == ns) && d.Name != "" {
			owned = append(owned, d)
		}
	}
	return owned, nil
}

func (p *phoneSupervisor) reconcile() error {
	phones, err := p.loadPhones()
	if err != nil {
		return err
	}
	p.mu.Lock()
	var gone []string
	for ns, sp := range p.spaces {
		if phones[ns] != sp.phone {
			gone = append(gone, ns)
		}
	}
	p.mu.Unlock()
	for _, ns := range gone {
		// Withdrawn or replaced: nothing may keep waiting on its behalf, and
		// the old phone's answers are refused from here on (card.phone).
		p.teardown(ns, true)
	}

	namespaces := make([]string, 0, len(phones))
	for ns := range phones {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)
	for _, ns := range namespaces {
		if err := p.reconcileSpace(ns, phones[ns]); err != nil {
			p.logf("approval phone %s: %v", ns, err)
		}
	}
	p.pruneCards()
	return nil
}

func (p *phoneSupervisor) reconcileSpace(ns, phone string) error {
	devices, err := p.ownedDevices(ns)
	if err != nil {
		return err
	}
	var phoneRow *phoneDeviceRow
	online := map[string]bool{}
	for i := range devices {
		if devices[i].Name == phone {
			phoneRow = &devices[i]
		}
		if devices[i].Online {
			online[devices[i].Name] = true
		}
	}

	p.mu.Lock()
	sp := p.spaces[ns]
	if sp == nil {
		sp = &phoneSpace{ns: ns, phone: phone, targets: map[string]*phoneTarget{}}
		p.spaces[ns] = sp
	}
	sp.names = map[string]string{}
	for _, d := range devices {
		if d.DisplayName != "" {
			sp.names[d.Name] = d.DisplayName
		}
	}
	if phoneRow != nil {
		sp.phoneName = sp.displayName(phone)
	}
	wasOnline := sp.online
	p.mu.Unlock()

	if phoneRow == nil || !phoneRow.Online {
		if wasOnline {
			p.teardown(ns, false)
		}
		return nil
	}
	if !wasOnline {
		if err := p.bringUp(ns, phone); err != nil {
			return fmt.Errorf("phone %s not reachable: %w", phone, err)
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if sp = p.spaces[ns]; sp == nil || !sp.online {
		return nil
	}
	for device := range online {
		if sp.targets[device] == nil {
			p.startTargetLocked(sp, device)
		}
	}
	for device, t := range sp.targets {
		if !online[device] {
			// Gone from the registry. Its session is already dead or about
			// to be; the watch would only keep re-dialling a device that is
			// not there.
			t.cancel()
			delete(sp.targets, device)
		}
	}
	return nil
}

// bringUp confirms the phone answers on a console session before anything
// starts waiting on it — the relay registry says online for up to 40 seconds
// after a phone lost its network — then starts listening for its decisions.
//
// The session is the link's own, not the pooled one every portal page shares:
// a page whose status query times out closes the pooled connection, and on a
// real phone that tore the watch down every few seconds (S13, 2026-09-30).
func (p *phoneSupervisor) bringUp(ns, phone string) error {
	ctx, cancel := context.WithTimeout(p.ctx, phonePushTimeout)
	session, closeSession, err := p.linkFor(ctx, ns, phone)
	cancel()
	if err != nil {
		return err
	}
	check := make(chan error, 1)
	go func() { _, err := session.state(); check <- err }()
	select {
	case err := <-check:
		if err != nil {
			closeSession()
			return err
		}
	case <-time.After(phonePushTimeout):
		closeSession()
		return errors.New("no answer")
	}

	replies, unsubscribe := session.replies()
	linkCtx, linkCancel := context.WithCancel(p.ctx)
	p.mu.Lock()
	sp := p.spaces[ns]
	if sp == nil || sp.phone != phone {
		p.mu.Unlock()
		linkCancel()
		unsubscribe()
		closeSession()
		return nil
	}
	sp.online = true
	sp.session = session
	sp.link = func() {
		linkCancel()
		closeSession()
	}
	p.mu.Unlock()
	p.logf("approval phone %s/%s online", ns, phone)

	p.launch(func(context.Context) {
		defer unsubscribe()
		for {
			select {
			case <-linkCtx.Done():
				return
			case m, ok := <-replies:
				if !ok {
					// The phone's session died. Stop waiting on its behalf
					// now rather than at the next reconcile, then look again.
					p.phoneLost(ns, phone, errors.New("console session closed"))
					return
				}
				p.handleReply(ns, phone, m)
			}
		}
	})
	return nil
}

func (sp *phoneSpace) displayName(device string) string {
	if n := sp.names[device]; n != "" {
		return n
	}
	return device
}

func (p *phoneSupervisor) startTargetLocked(sp *phoneSpace, device string) {
	ctx, cancel := context.WithCancel(p.ctx)
	t := &phoneTarget{cancel: cancel, done: make(chan struct{})}
	sp.targets[device] = t
	ns := sp.ns
	seenPending := map[string]string{} // pending id -> card id
	seenPairing := map[string]string{} // controller fp -> card id
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer close(t.done)
		_ = residentWatch{
			ns: ns, device: device, wait: phoneApprovalWait,
			sessionFor: func(ctx context.Context, ns, device string) (deviceSession, error) {
				return p.sessionFor(ctx, ns, device)
			},
			retryMin:  p.retryMin,
			retryMax:  p.retryMax,
			waitRetry: p.waitRetryFn,
			logf:      p.logf,
			logPrefix: "approval phone",
			onDial:    func(error) error { return nil },
			onState: func(ctx context.Context, session deviceSession, state console.State, wait time.Duration) {
				p.onTargetState(ns, device, session, state, wait, seenPending, seenPairing)
			},
		}.run(ctx)
	}()
}

// onTargetState pushes what is new and settles what is gone.
func (p *phoneSupervisor) onTargetState(ns, device string, session deviceSession, state console.State, wait time.Duration, seenPending, seenPairing map[string]string) {
	names := map[string]string{}
	for _, c := range state.Trusted {
		names[c.FP] = c.Name
	}
	current := map[string]bool{}
	for _, pend := range state.Pending {
		current[pend.ID] = true
		if _, ok := seenPending[pend.ID]; ok {
			continue
		}
		pc := p.newCard(ns, device, protocol.ApprovalCard{
			Kind: pend.Kind, Peer: names[pend.Peer], PeerFP: pend.Peer,
			Cmd: pend.Cmd, Path: pend.Path, Cwd: pend.Cwd, Created: pend.Created,
		}, wait)
		if pc == nil {
			continue
		}
		pc.pendingID = pend.ID
		seenPending[pend.ID] = pc.card.ID
		if err := p.push(ns, pc.phone, pc.card); err != nil {
			// Nobody will see it: refuse it now, as the device would have
			// without a watch, instead of letting the controller hang.
			if derr := session.decide(pend.ID, "n", "system:approval-phone-unreachable"); derr != nil {
				p.logf("approval phone %s: refuse %s on %s: %v", ns, pend.ID, device, derr)
			}
			p.phoneLost(ns, pc.phone, err)
		}
	}
	for id, cardID := range seenPending {
		if !current[id] {
			delete(seenPending, id)
			p.settle(cardID)
		}
	}

	currentPairs := map[string]bool{}
	for _, pair := range state.PendingPairings {
		currentPairs[pair.FP] = true
		if _, ok := seenPairing[pair.FP]; ok {
			continue
		}
		peer := pair.Name
		if pair.Label != "" && pair.Label != pair.Name {
			peer = pair.Name + "（" + pair.Label + "）"
		}
		pc := p.newCard(ns, device, protocol.ApprovalCard{
			Kind: "pair", Peer: peer, PeerFP: pair.FP, Created: pair.Created,
		}, phonePairingWindow)
		if pc == nil {
			continue
		}
		pc.pairFP = pair.FP
		seenPairing[pair.FP] = pc.card.ID
		if err := p.push(ns, pc.phone, pc.card); err != nil {
			p.phoneLost(ns, pc.phone, err)
		}
	}
	for fp, cardID := range seenPairing {
		if !currentPairs[fp] {
			delete(seenPairing, fp)
			p.settle(cardID)
		}
	}
}

// newCard records a pending card addressed to the namespace's current phone.
// It returns nil when there is no phone to address it to.
func (p *phoneSupervisor) newCard(ns, device string, card protocol.ApprovalCard, window time.Duration) *phoneCard {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil
	}
	now := p.now()
	card.ID = hex.EncodeToString(b[:])
	card.State = protocol.ApprovalPending
	card.Expires = now.Add(window)
	if card.Created.IsZero() {
		card.Created = now
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	sp := p.spaces[ns]
	if sp == nil {
		return nil
	}
	card.Device = sp.displayName(device)
	pc := &phoneCard{card: card, ns: ns, device: device, phone: sp.phone, seenAt: now}
	p.cards[card.ID] = pc
	return pc
}

// settle runs when a request is no longer on its device. If the owner did not
// answer it from the phone, either its wait ran out (the card turns into a late
// approval) or someone answered it elsewhere (the card is done).
func (p *phoneSupervisor) settle(cardID string) {
	p.mu.Lock()
	pc := p.cards[cardID]
	if pc == nil || pc.answered || pc.card.State != protocol.ApprovalPending {
		p.mu.Unlock()
		return
	}
	if !p.now().Before(pc.card.Expires.Add(-phoneExpirySlack)) {
		pc.card.State = protocol.ApprovalExpired
	} else {
		pc.card.State = protocol.ApprovalDone
		pc.card.Result = protocol.ResultHandled
		pc.answered = true
	}
	card, ns, phone := pc.card, pc.ns, pc.phone
	p.mu.Unlock()
	if err := p.push(ns, phone, card); err != nil {
		// Best effort: the phone keeps the older state, and a decision it
		// sends later is judged against this record, not against the card.
		p.logf("approval phone %s: update card: %v", ns, err)
	}
}

// answeredElsewhere tells a request that left its device before its wait ran
// out from one that expired, by the rule settle uses. It also counts a request
// a restarting device dropped as answered, so the phone's approval lets
// nothing through: wrong, but in the safe direction (ADR 0015).
func (p *phoneSupervisor) answeredElsewhere(pc phoneCard) bool {
	return p.now().Before(pc.card.Expires.Add(-phoneExpirySlack))
}

// push sends a card to the phone over its link, if it is still that
// namespace's phone and the link is up.
func (p *phoneSupervisor) push(ns, phone string, card protocol.ApprovalCard) error {
	p.mu.Lock()
	var session phoneSession
	if sp := p.spaces[ns]; sp != nil && sp.phone == phone && sp.online {
		session = sp.session
	}
	p.mu.Unlock()
	if session == nil {
		return errPhoneLinkDown
	}
	return session.approvalPush(card, phonePushTimeout)
}

// errPhoneLinkDown: the phone is not linked right now, so a card has nowhere
// to go.
var errPhoneLinkDown = errors.New("approval phone not linked")

// handleReply applies a decision the owner made on the phone. It is accepted
// only for a card this portal pushed to that very phone, which is still the
// namespace's phone, once, and within the retention window (ADR 0015).
func (p *phoneSupervisor) handleReply(ns, phone string, m protocol.Message) {
	id, verdict := m.ApprovalID, m.Verdict
	if verdict != "y" && verdict != "n" {
		return
	}
	p.mu.Lock()
	sp := p.spaces[ns]
	pc := p.cards[id]
	valid := pc != nil && sp != nil && sp.phone == phone && pc.ns == ns && pc.phone == phone &&
		p.now().Sub(pc.seenAt) < phoneCardRetention
	if !valid {
		p.mu.Unlock()
		// Answer anyway, so the phone stops resending and says so.
		_ = p.push(ns, phone, protocol.ApprovalCard{ID: id, State: protocol.ApprovalDone, Result: protocol.ResultGone})
		return
	}
	if pc.answered {
		// A resend after a reconnect: repeat how it ended.
		card := pc.card
		p.mu.Unlock()
		_ = p.push(ns, phone, card)
		return
	}
	pc.answered = true
	approver := "phone:" + sp.phoneName
	snap := *pc
	p.mu.Unlock()

	result := p.apply(snap, verdict, approver)

	p.mu.Lock()
	pc.card.State = protocol.ApprovalDone
	pc.card.Result = result
	card := pc.card
	p.mu.Unlock()
	p.logf("approval phone %s: %s %s on %s by %s: %s", ns, snap.card.Kind, verdictWord(verdict), snap.card.Device, approver, result)
	if err := p.push(ns, phone, card); err != nil {
		p.logf("approval phone %s: confirm card: %v", ns, err)
	}
}

func verdictWord(v string) string {
	if v == "y" {
		return "allow"
	}
	return "deny"
}

// apply carries a decision to the target device and says how it ended.
func (p *phoneSupervisor) apply(pc phoneCard, verdict, approver string) string {
	ctx, cancel := context.WithTimeout(p.ctx, phonePushTimeout)
	defer cancel()
	session, err := p.sessionFor(ctx, pc.ns, pc.device)
	if err != nil {
		p.logf("approval phone %s: reach %s: %v", pc.ns, pc.device, err)
		return protocol.ResultGone
	}
	if pc.pairFP != "" {
		if pc.card.State != protocol.ApprovalPending {
			return protocol.ResultGone
		}
		switch err := session.pairDecide(pc.pairFP, verdict); {
		case errors.Is(err, errPairingGone):
			if p.answeredElsewhere(pc) {
				return protocol.ResultHandled
			}
			return protocol.ResultGone
		case err != nil:
			p.logf("approval phone %s: pairing on %s: %v", pc.ns, pc.card.Device, err)
			return protocol.ResultGone
		case verdict == "y":
			return protocol.ResultAllowed
		default:
			return protocol.ResultDenied
		}
	}
	if pc.card.State == protocol.ApprovalPending {
		found, err := session.decideFound(pc.pendingID, verdict, approver)
		if err != nil {
			p.logf("approval phone %s: decide on %s: %v", pc.ns, pc.card.Device, err)
			return protocol.ResultGone
		}
		if found {
			if verdict == "y" {
				return protocol.ResultAllowed
			}
			return protocol.ResultDenied
		}
		if p.answeredElsewhere(pc) {
			// Gone before its wait ran out: someone answered it on the
			// portal or at the device first, and that answer stands. A late
			// grant here would let the command through a second time.
			return protocol.ResultHandled
		}
		// The wait ran out between the push and the tap: same as expired.
	}
	if verdict != "y" {
		return protocol.ResultDenied
	}
	label := pc.card.Cmd
	if pc.card.Kind != "exec" && pc.card.Kind != "exec-elevated" {
		label = pc.card.Path
	}
	if err := session.grantOnce(pc.card.Kind, label, pc.card.PeerFP, protocol.LateGrantWindow, approver); err != nil {
		p.logf("approval phone %s: late grant on %s: %v", pc.ns, pc.card.Device, err)
		return protocol.ResultGone
	}
	return protocol.ResultGranted
}

// phoneLost stops waiting on the phone's behalf. It returns at once; the
// teardown runs on its own goroutine because a target watch may be the caller.
func (p *phoneSupervisor) phoneLost(ns, phone string, cause error) {
	p.mu.Lock()
	sp := p.spaces[ns]
	lost := sp != nil && sp.phone == phone && sp.online
	p.mu.Unlock()
	if !lost {
		return
	}
	p.logf("approval phone %s/%s offline: %v", ns, phone, cause)
	p.launch(func(context.Context) {
		p.teardown(ns, false)
		p.triggerReconcile()
	})
}

// teardown stops every watch in ns and closes the sessions they held, so the
// devices have no front-end and refuse unwatched requests at once again, as
// they did before the phone was set. forget also drops the namespace record.
func (p *phoneSupervisor) teardown(ns string, forget bool) {
	p.mu.Lock()
	sp := p.spaces[ns]
	if sp == nil {
		p.mu.Unlock()
		return
	}
	targets := sp.targets
	sp.targets = map[string]*phoneTarget{}
	sp.online = false
	link := sp.link
	sp.link = nil
	sp.session = nil
	if forget {
		delete(p.spaces, ns)
	}
	p.mu.Unlock()
	if link != nil {
		link()
	}
	for device, t := range targets {
		t.cancel()
		<-t.done
		p.dropConn(ns, device)
	}
}

func (p *phoneSupervisor) stopAll() {
	p.mu.Lock()
	namespaces := make([]string, 0, len(p.spaces))
	for ns := range p.spaces {
		namespaces = append(namespaces, ns)
	}
	p.mu.Unlock()
	for _, ns := range namespaces {
		p.teardown(ns, true)
	}
}

// pruneCards drops records past retention and keeps each namespace under its
// cap, oldest first. A decision naming a dropped card is answered "gone".
func (p *phoneSupervisor) pruneCards() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	byNS := map[string][]*phoneCard{}
	for id, pc := range p.cards {
		if now.Sub(pc.seenAt) >= phoneCardRetention {
			delete(p.cards, id)
			continue
		}
		byNS[pc.ns] = append(byNS[pc.ns], pc)
	}
	for _, list := range byNS {
		if len(list) <= phoneMaxCards {
			continue
		}
		sort.Slice(list, func(i, j int) bool { return list[i].seenAt.Before(list[j].seenAt) })
		for _, pc := range list[:len(list)-phoneMaxCards] {
			delete(p.cards, pc.card.ID)
		}
	}
}

// status is what the portal shows for a namespace: which device is the phone,
// and whether approvals are reaching it right now.
func (p *phoneSupervisor) status(ns string) (device string, online bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if sp := p.spaces[ns]; sp != nil {
		return sp.phone, sp.online
	}
	return "", false
}

// errPhoneIncapable: the device answered but refused the push, i.e. it is not
// an agent hosted by the Android app (or that app is too old).
var errPhoneIncapable = errors.New("device cannot take approvals")

// testPush sends the designation's test card. It doubles as the check that
// the device can take approvals at all: only an agent hosted by the Android
// app accepts approval_push. Any error other than errPhoneIncapable means the
// device could not be reached.
func (p *phoneSupervisor) testPush(ctx context.Context, ns, device, name string) error {
	ctx, cancel := context.WithTimeout(ctx, phonePushTimeout)
	defer cancel()
	session, err := p.sessionFor(ctx, ns, device)
	if err != nil {
		return err
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	err = session.approvalPush(protocol.ApprovalCard{
		ID: hex.EncodeToString(b[:]), State: protocol.ApprovalTest, Device: name, Created: p.now(),
	}, phonePushTimeout)
	var refused *deviceRefusedError
	if errors.As(err, &refused) {
		return fmt.Errorf("%w: %s", errPhoneIncapable, refused.reason)
	}
	return err
}
