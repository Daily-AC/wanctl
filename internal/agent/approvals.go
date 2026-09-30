package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"wanctl/internal/eventlog"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
	"wanctl/internal/transport"
)

// The approval phone (ADR 0015, decision 3). The wanctl Android app runs this
// agent as a child process with --approvals-stdio. The portal pushes approval
// cards over its console session and the agent hands each one to the app as a
// stdout line; the owner's decision comes back as a stdin line and goes to the
// portal, unsolicited, on the same console session. No port, socket or Android
// IPC surface is involved: the approval rides the connection the phone
// already keeps.
//
// Late approvals (decision 2) live here too: the one-shot grant the portal
// installs on any device, phone or not, when the owner says yes to a request
// whose wait had already run out.

// approvalLinePrefix starts every stdout line the app turns into a
// notification. Nothing else the agent prints may start a line with it from
// text a controller chose. Controller names reach stdout only through %q
// (authorize, Run), which escapes a line break, and peerText has already
// stripped control characters from them at hello; keep it that way. The card
// itself is marshalled again before it is printed, so a line break inside a
// command stays an escape and the card is always exactly one line.
const approvalLinePrefix = "wanctl-approval "

// errNotApprovalPhone is how the portal tells a phone running the app from
// every other device: it designates only a device that accepts a test push.
const errNotApprovalPhone = "this device cannot take approvals: only the wanctl Android app runs its agent with --approvals-stdio"

const (
	// maxApprovalCard bounds one pushed card. A card is a command label, a
	// path and a few names; a card this large is a portal bug, and the app
	// would have to hold it whole to parse the line.
	maxApprovalCard = 16 << 10

	// maxDecisionLine bounds one stdin line. A decision is an id and a
	// letter; anything longer is skipped, not buffered.
	maxDecisionLine = 1 << 10

	// maxOutbox and outboxTTL bound the decisions kept for resending. The
	// portal answers every decision with a done card within seconds of
	// seeing it, so the outbox only grows while the phone is offline, and an
	// hour is longer than any sensible reconnect.
	maxOutbox = 64
	outboxTTL = time.Hour
)

// approvalPhone is the agent's half of the stdio link to the Android app. It
// exists only with Options.ApprovalsStdio.
type approvalPhone struct {
	out   io.Writer
	outMu sync.Mutex // one card per write, never two interleaved

	in        io.Reader
	done      <-chan struct{} // the agent's shutdown
	readOnce  sync.Once
	decisions chan phoneDecision

	mu       sync.Mutex
	now      func() time.Time // nil means time.Now; tests move the clock
	outbox   map[string]phoneDecision
	consoles map[int]func(protocol.Message) error // live console sessions' senders
	nextID   int
}

// phoneDecision is one decision the owner made in the app.
type phoneDecision struct {
	id, verdict string
	at          time.Time
}

func (d phoneDecision) reply() protocol.Message {
	return protocol.Message{Kind: protocol.KindApprovalReply, ApprovalID: d.id, Verdict: d.verdict}
}

func newApprovalPhone(out io.Writer, in io.Reader, done <-chan struct{}) *approvalPhone {
	return &approvalPhone{
		out: out, in: in, done: done,
		decisions: make(chan phoneDecision, 8),
		outbox:    map[string]phoneDecision{},
		consoles:  map[int]func(protocol.Message) error{},
	}
}

func (p *approvalPhone) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// approvalPush hands a card from the portal to the app. A device that is not
// the app's child refuses, which is what keeps it from being designated.
func (a *Agent) approvalPush(data json.RawMessage) protocol.Message {
	if a.phone == nil {
		return protocol.Message{Kind: protocol.KindError, Reason: errNotApprovalPhone}
	}
	card, err := decodeApprovalCard(data)
	if err != nil {
		return protocol.Message{Kind: protocol.KindError, Reason: err.Error()}
	}
	if err := a.phone.show(card); err != nil {
		return protocol.Message{Kind: protocol.KindError, Reason: "approval card not handed to the app: " + err.Error()}
	}
	return protocol.Message{Kind: protocol.KindApprovalPush}
}

func decodeApprovalCard(data json.RawMessage) (protocol.ApprovalCard, error) {
	var card protocol.ApprovalCard
	if len(data) > maxApprovalCard {
		return card, fmt.Errorf("approval card too large: %d bytes, limit %d", len(data), maxApprovalCard)
	}
	if err := json.Unmarshal(data, &card); err != nil {
		return card, fmt.Errorf("approval card is not a card object: %v", err)
	}
	if card.ID == "" {
		return card, errors.New("approval card has no id")
	}
	switch card.State {
	case protocol.ApprovalPending, protocol.ApprovalExpired, protocol.ApprovalDone, protocol.ApprovalTest:
	default:
		return card, fmt.Errorf("approval card has unknown state %q", card.State)
	}
	return card, nil
}

// show writes one card as one line. A done card also settles the decision
// the outbox was holding for it: the portal has a final answer, so resending
// one would only be discarded.
func (p *approvalPhone) show(card protocol.ApprovalCard) error {
	b, err := json.Marshal(card)
	if err != nil {
		return err
	}
	if card.State == protocol.ApprovalDone {
		p.mu.Lock()
		delete(p.outbox, card.ID)
		p.mu.Unlock()
	}
	line := make([]byte, 0, len(approvalLinePrefix)+len(b)+1)
	line = append(append(append(line, approvalLinePrefix...), b...), '\n')
	p.outMu.Lock()
	defer p.outMu.Unlock()
	_, err = p.out.Write(line)
	return err
}

// serveDecisions delivers the owner's decisions from the app until ctx ends.
//
// The stdin read itself runs on a goroutine the agent does not own: a
// blocking read on a pipe cannot be interrupted, so Close could never join it.
// That goroutine touches nothing but the decisions channel. It ends at stdin's
// EOF, or the next time it has a decision to hand on after the agent shut
// down; until then it stays parked in the read, which production ends by
// exiting.
func (p *approvalPhone) serveDecisions(ctx context.Context) {
	p.readOnce.Do(func() { go p.readDecisions() })
	for {
		select {
		case <-ctx.Done():
			return
		case d, ok := <-p.decisions:
			if !ok {
				return // the app closed stdin: nothing more will come, and that is not an error
			}
			p.decide(d)
		}
	}
}

func (p *approvalPhone) readDecisions() {
	defer close(p.decisions)
	br := bufio.NewReaderSize(p.in, maxDecisionLine)
	for {
		line, err := br.ReadSlice('\n')
		for errors.Is(err, bufio.ErrBufferFull) {
			// Longer than any decision: drop the rest of it and pick up
			// again at the next line, rather than ending the reader.
			line = nil
			_, err = br.ReadSlice('\n')
		}
		if d, ok := parseDecision(line); ok {
			select {
			case p.decisions <- d:
			case <-p.done:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// parseDecision reads {"id":"…","verdict":"y"|"n"}. Anything else is ignored:
// the app is the only writer, and a line it got wrong is better dropped than
// guessed at.
func parseDecision(line []byte) (phoneDecision, bool) {
	var in struct {
		ID      string `json:"id"`
		Verdict string `json:"verdict"`
	}
	if json.Unmarshal(bytes.TrimSpace(line), &in) != nil || in.ID == "" {
		return phoneDecision{}, false
	}
	if in.Verdict != "y" && in.Verdict != "n" {
		return phoneDecision{}, false
	}
	return phoneDecision{id: in.ID, verdict: in.Verdict}, true
}

// decide keeps a decision until the portal settles it and sends it on every
// live console session now. The agent does not check the id against the
// cards it showed: the app may have outlived an agent restart, and the portal,
// which issued the id, is the one that decides whether it still means anything.
func (p *approvalPhone) decide(d phoneDecision) {
	p.mu.Lock()
	d.at = p.clock()
	p.pruneOutboxLocked(d.at)
	if _, known := p.outbox[d.id]; !known && len(p.outbox) >= maxOutbox {
		oldest := ""
		for id, o := range p.outbox {
			if oldest == "" || o.at.Before(p.outbox[oldest].at) {
				oldest = id
			}
		}
		delete(p.outbox, oldest)
	}
	p.outbox[d.id] = d // a later decision for the same id replaces the earlier one
	sends := make([]func(protocol.Message) error, 0, len(p.consoles))
	for _, send := range p.consoles {
		sends = append(sends, send)
	}
	p.mu.Unlock()
	for _, send := range sends {
		_ = send(d.reply()) // a session that fails is ending; the next one gets the resend
	}
}

// attach registers a console session and resends it every decision the
// portal has not settled yet: one made while the phone had no network reaches
// the portal the moment the phone is back.
func (p *approvalPhone) attach(send func(protocol.Message) error) (detach func()) {
	p.mu.Lock()
	id := p.nextID
	p.nextID++
	p.consoles[id] = send
	p.pruneOutboxLocked(p.clock())
	unsettled := make([]phoneDecision, 0, len(p.outbox))
	for _, d := range p.outbox {
		unsettled = append(unsettled, d)
	}
	p.mu.Unlock()
	slices.SortFunc(unsettled, func(x, y phoneDecision) int { return x.at.Compare(y.at) })
	for _, d := range unsettled {
		_ = send(d.reply())
	}
	return func() {
		p.mu.Lock()
		delete(p.consoles, id)
		p.mu.Unlock()
	}
}

func (p *approvalPhone) pruneOutboxLocked(now time.Time) {
	for id, d := range p.outbox {
		if now.Sub(d.at) >= outboxTTL {
			delete(p.outbox, id)
		}
	}
}

const (
	// maxLateGrants bounds the grants waiting for their retry. Each is one
	// approval the owner made in the last half hour; past this, the oldest
	// goes, and its retry simply asks again.
	maxLateGrants = 32

	// maxAskedScripts and askedScriptTTL bound what the device remembers of
	// the scripts it asked about (see resolveLocked). A day is as long as the
	// portal accepts a decision (ADR 0015, decision 1).
	maxAskedScripts = 64
	askedScriptTTL  = 24 * time.Hour
)

// lateGrants holds the one-shot grants late approvals install (ADR 0015,
// decision 2). It lives in memory only: an agent restart drops every grant,
// which fails safe. The zero value is ready to use.
type lateGrants struct {
	mu      sync.Mutex
	now     func() time.Time // nil means time.Now; tests move the clock
	grants  []*lateGrant     // in install order
	scripts []askedScript
}

// lateGrant lets one request through: the same kind, from the same
// controller, for the same command or path.
type lateGrant struct {
	kind    policy.Kind
	fp      string
	key     string // grantKey of the request it releases
	expires time.Time
}

// askedScript is a script the device asked the owner about: the label the
// card showed and the token that label abbreviates.
type askedScript struct {
	kind         policy.Kind
	fp           string
	label, token string
	at           time.Time
}

func (g *lateGrants) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// grantKey is what a grant compares against a request: the whole command
// (for a script, its whole token) or the path. ok is false for a kind no
// grant can release.
func grantKey(req policy.Request) (key string, ok bool) {
	switch req.Kind {
	case policy.KindExec, policy.KindExecElevated:
		return policy.CommandPattern(req.Cmd), true
	case policy.KindRead, policy.KindWrite:
		return req.Path, true
	case policy.KindLogs:
		return "", true
	}
	return "", false
}

// grantOnce installs a late approval sent by the portal and records it on
// this device, which is where its use is recorded too.
func (a *Agent) grantOnce(msg protocol.Message) protocol.Message {
	window := lateGrantWindow(msg.TimeoutSec)
	if err := a.grants.install(policy.Kind(msg.RuleKind), msg.Pattern, msg.FP, msg.Approver, window); err != nil {
		return protocol.Message{Kind: protocol.KindError, Reason: err.Error()}
	}
	if a.log != nil {
		a.log.Append(eventlog.Event{
			Type: "connect", PeerFP: msg.FP,
			Detail: fmt.Sprintf("late approval %s %s by %s, once within %s", msg.RuleKind, msg.Pattern, msg.Approver, shortDuration(window)),
		})
	}
	return protocol.Message{Kind: protocol.KindGrantOnce}
}

// lateGrantWindow clamps what the portal asked for to at most
// protocol.LateGrantWindow; zero means that window.
func lateGrantWindow(sec int) time.Duration {
	// Compared as seconds: a huge TimeoutSec would overflow as a Duration.
	switch max := int(protocol.LateGrantWindow / time.Second); {
	case sec == 0 || sec > max:
		return protocol.LateGrantWindow
	case sec < 1:
		return time.Second
	}
	return time.Duration(sec) * time.Second
}

func shortDuration(d time.Duration) string {
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return d.String()
}

func (g *lateGrants) install(kind policy.Kind, label, fp, approver string, window time.Duration) error {
	switch kind {
	case policy.KindExec, policy.KindExecElevated, policy.KindRead, policy.KindWrite:
		if label == "" {
			return fmt.Errorf("a late approval for %s must name the command or path it releases", kind)
		}
	case policy.KindLogs:
		if label != "" {
			return errors.New("a late approval for logs names no command or path")
		}
	default:
		return fmt.Errorf("unknown request kind %q", kind)
	}
	if !transport.ValidFingerprint(fp) {
		return fmt.Errorf("invalid controller fingerprint %q", fp)
	}
	if approver == "" {
		return errors.New("a late approval must name its approver")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock()
	g.pruneLocked(now)
	key := label
	if kind == policy.KindExec || kind == policy.KindExecElevated {
		var err error
		if key, err = g.resolveLocked(kind, fp, label); err != nil {
			return err
		}
	}
	// The same approval twice still releases the request once.
	g.grants = slices.DeleteFunc(g.grants, func(x *lateGrant) bool {
		return x.kind == kind && x.fp == fp && x.key == key
	})
	if len(g.grants) >= maxLateGrants {
		g.grants = slices.Delete(g.grants, 0, 1)
	}
	g.grants = append(g.grants, &lateGrant{kind: kind, fp: fp, key: key, expires: now.Add(window)})
	return nil
}

// resolveLocked turns the command label a card showed into what the grant
// compares. For a script that label is an abbreviation of its token
// (policy.CommandLabel), and nothing may match on it: two scripts sharing the
// visible prefix cost a controller about 2^32 work (internal/script,
// Canonical), after which the benign one's approval would run the other. So a
// script label is looked up among the scripts this device asked about, and the
// grant carries the whole token. A label two different scripts from the same
// controller share is refused rather than guessed; that is the attack, or a
// coincidence nobody will meet. A label that names nothing asked about is kept
// as it is. That is right for an ordinary command, whose label is the command,
// and it never matches a script, whose token is not an abbreviation, so an
// agent that restarted since it asked fails safe. Caller holds g.mu.
func (g *lateGrants) resolveLocked(kind policy.Kind, fp, label string) (string, error) {
	token := ""
	for _, s := range g.scripts {
		if s.kind != kind || s.fp != fp || s.label != label {
			continue
		}
		if token != "" && token != s.token {
			return "", fmt.Errorf("%s names more than one script this controller asked to run; approve it while it is still waiting instead", label)
		}
		token = s.token
	}
	if token == "" {
		return label, nil
	}
	return token, nil
}

// asked records a script the device is about to ask the owner about, so a
// late approval of its card can be bound to the whole script. An ordinary
// command needs no record: its label is the whole command.
func (g *lateGrants) asked(req policy.Request) {
	if req.Kind != policy.KindExec && req.Kind != policy.KindExecElevated {
		return
	}
	label, token := policy.CommandLabel(req.Cmd), policy.CommandPattern(req.Cmd)
	if label == token {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock()
	g.pruneLocked(now)
	g.scripts = slices.DeleteFunc(g.scripts, func(s askedScript) bool {
		return s.kind == req.Kind && s.fp == req.Peer && s.token == token
	})
	if len(g.scripts) >= maxAskedScripts {
		g.scripts = slices.Delete(g.scripts, 0, 1)
	}
	g.scripts = append(g.scripts, askedScript{kind: req.Kind, fp: req.Peer, label: label, token: token, at: now})
}

// find reports the grant that would release req, without spending it.
func (g *lateGrants) find(req policy.Request) *lateGrant {
	key, ok := grantKey(req)
	if !ok || req.Peer == "" {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneLocked(g.clock())
	for _, x := range g.grants {
		if x.kind == req.Kind && x.fp == req.Peer && x.key == key {
			return x
		}
	}
	return nil
}

// take spends a grant find returned. It reports false if the grant expired or
// another request spent it in between.
func (g *lateGrants) take(grant *lateGrant) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneLocked(g.clock())
	n := len(g.grants)
	g.grants = slices.DeleteFunc(g.grants, func(x *lateGrant) bool { return x == grant })
	return len(g.grants) < n
}

func (g *lateGrants) pruneLocked(now time.Time) {
	g.grants = slices.DeleteFunc(g.grants, func(x *lateGrant) bool { return !now.Before(x.expires) })
	g.scripts = slices.DeleteFunc(g.scripts, func(s askedScript) bool { return now.Sub(s.at) >= askedScriptTTL })
}

// useLateGrant lets req through on a late approval, if one matches. handled
// is false when none does, and the owner is asked as usual. A grant is spent
// only on a request that may otherwise proceed, so a delegation that lapsed in
// between does not burn it.
func (a *Agent) useLateGrant(req policy.Request, checks []func() bool) (ok bool, decision string, handled bool) {
	grant := a.grants.find(req)
	if grant == nil {
		return false, "", false
	}
	if !checksPass(checks) {
		return false, "delegation inactive", true
	}
	if !a.grants.take(grant) {
		return false, "", false
	}
	return true, "late-approved", true
}
