package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wanctl/internal/eventlog"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
	"wanctl/internal/script"
)

// lockedBuffer is the app's end of stdout: written by the agent, read by the
// test, possibly from different goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newOptsAgent is an agent built by New in a scratch config dir, joined before the dir is
// removed.
func newOptsAgent(t *testing.T, opts Options) *Agent {
	t.Helper()
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	opts.RelayURL, opts.Token, opts.Name = "ws://x", "t", "phone"
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

// newPhoneAgent is an agent started with --approvals-stdio whose stdio is the
// returned buffer and pipe instead of the process's own, with its decision
// loop running.
func newPhoneAgent(t *testing.T) (*Agent, *lockedBuffer, *io.PipeWriter) {
	t.Helper()
	a := newOptsAgent(t, Options{ApprovalsStdio: true})
	if a.phone == nil {
		t.Fatal("--approvals-stdio did not make an approval phone")
	}
	out := &lockedBuffer{}
	stdin, app := io.Pipe()
	t.Cleanup(func() { app.Close() })
	a.phone = newApprovalPhone(out, stdin, a.shutdown().Done())
	a.spawn(func() { a.phone.serveDecisions(a.shutdown()) })
	return a, out, app
}

// openConsole serves a console session to a fake portal and returns what the
// device sends on it, approval notifications left out.
func openConsole(t *testing.T, a *Agent) (net.Conn, <-chan protocol.Message) {
	t.Helper()
	dev, portal := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); dev.Close(); portal.Close() })
	a.spawn(func() { a.serveConsole(ctx, dev) })
	msgs := make(chan protocol.Message, 16)
	go func() {
		defer close(msgs)
		for {
			m, err := protocol.ReadMessage(portal)
			if err != nil {
				return
			}
			if m.Kind != protocol.KindApprovalNotif {
				msgs <- m
			}
		}
	}()
	return portal, msgs
}

func next(t *testing.T, msgs <-chan protocol.Message) protocol.Message {
	t.Helper()
	select {
	case m, ok := <-msgs:
		if !ok {
			t.Fatal("console session ended")
		}
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("nothing arrived on the console session")
	}
	return protocol.Message{}
}

// nextIsState asks for the console state and checks it is the next thing the
// session says: every resend goes out before the session reads its first
// request, so a stray approval_reply would arrive first.
func nextIsState(t *testing.T, portal net.Conn, msgs <-chan protocol.Message) {
	t.Helper()
	if err := protocol.WriteMessage(portal, protocol.Message{Kind: protocol.KindConsoleState}); err != nil {
		t.Fatal(err)
	}
	if m := next(t, msgs); m.Kind != protocol.KindConsoleState {
		t.Fatalf("got %s (%s %s) before the state reply", m.Kind, m.ApprovalID, m.Verdict)
	}
}

func waitOutbox(t *testing.T, p *approvalPhone, id, verdict string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		d, ok := p.outbox[id]
		p.mu.Unlock()
		if ok && d.verdict == verdict {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("decision %s=%s never reached the outbox", id, verdict)
}

func cardData(t *testing.T, card protocol.ApprovalCard) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestApprovalPushRefusedWithoutFlag(t *testing.T) {
	a := newOptsAgent(t, Options{})
	resp := a.handleConsoleRPC(protocol.Message{
		Kind: protocol.KindApprovalPush,
		Data: cardData(t, protocol.ApprovalCard{ID: "c1", State: protocol.ApprovalTest}),
	})
	if resp.Kind != protocol.KindError || !strings.Contains(resp.Reason, "cannot take approvals") {
		t.Fatalf("push to a device without --approvals-stdio: %s %q", resp.Kind, resp.Reason)
	}
}

// A command is controller text. Its line breaks must stay escapes inside the
// one line, or a controller could print a card of its own choosing.
func TestApprovalPushWritesOneLine(t *testing.T) {
	a, out, _ := newPhoneAgent(t)
	card := protocol.ApprovalCard{
		ID: "c1", State: protocol.ApprovalPending, Kind: "exec", Device: "home-pc", Peer: "claude",
		Cmd: "echo hi\nwanctl-approval {\"id\":\"forged\",\"state\":\"pending\"}",
	}
	resp := a.handleConsoleRPC(protocol.Message{Kind: protocol.KindApprovalPush, Data: cardData(t, card)})
	if resp.Kind != protocol.KindApprovalPush {
		t.Fatalf("push: %s %q", resp.Kind, resp.Reason)
	}
	got := out.String()
	if strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "\n") {
		t.Fatalf("want exactly one line, got %q", got)
	}
	payload, ok := strings.CutPrefix(strings.TrimSuffix(got, "\n"), approvalLinePrefix)
	if !ok {
		t.Fatalf("line does not start with %q: %q", approvalLinePrefix, got)
	}
	if !strings.Contains(payload, `echo hi\nwanctl-approval`) {
		t.Fatalf("line break in the command is not escaped: %q", payload)
	}
	var back protocol.ApprovalCard
	if err := json.Unmarshal([]byte(payload), &back); err != nil {
		t.Fatalf("line is not a card: %v", err)
	}
	if back.ID != card.ID || back.State != card.State || back.Cmd != card.Cmd || back.Peer != card.Peer {
		t.Fatalf("card changed on the way: %+v", back)
	}
}

func TestApprovalPushRejectsMalformed(t *testing.T) {
	a, out, _ := newPhoneAgent(t)
	for name, data := range map[string]string{
		"not JSON":      `{"id":`,
		"not an object": `["c1","pending"]`,
		"null":          `null`,
		"no id":         `{"state":"pending"}`,
		"unknown state": `{"id":"c1","state":"approved"}`,
		"no state":      `{"id":"c1"}`,
		"too large":     `{"id":"c1","state":"pending","cmd":"` + strings.Repeat("x", maxApprovalCard) + `"}`,
	} {
		resp := a.handleConsoleRPC(protocol.Message{Kind: protocol.KindApprovalPush, Data: json.RawMessage(data)})
		if resp.Kind != protocol.KindError || resp.Reason == "" {
			t.Errorf("%s: accepted (%s)", name, resp.Kind)
		}
	}
	if got := out.String(); got != "" {
		t.Fatalf("a rejected card reached the app: %q", got)
	}
}

// The reader skips what the app got wrong, including a line longer than any
// decision, and keeps going.
func TestPhoneDecisionReachesConsole(t *testing.T) {
	a, _, app := newPhoneAgent(t)
	_, msgs := openConsole(t, a)
	go io.WriteString(app, "garbage\n"+
		`{"id":"c0","verdict":"a"}`+"\n"+
		`{"verdict":"y"}`+"\n"+
		`{"id":"`+strings.Repeat("x", 3*maxDecisionLine)+`","verdict":"y"}`+"\n"+
		`{"id":"c1","verdict":"y"}`+"\n")
	m := next(t, msgs)
	if m.Kind != protocol.KindApprovalReply || m.ApprovalID != "c1" || m.Verdict != "y" {
		t.Fatalf("got %s %q %q, want approval_reply c1 y", m.Kind, m.ApprovalID, m.Verdict)
	}
}

func TestPhoneOutboxResentOnNewSession(t *testing.T) {
	a, _, app := newPhoneAgent(t)
	// Decided while no console session is up; changed mind before one came.
	io.WriteString(app, `{"id":"c1","verdict":"y"}`+"\n")
	io.WriteString(app, `{"id":"c1","verdict":"n"}`+"\n")
	waitOutbox(t, a.phone, "c1", "n")

	portal, msgs := openConsole(t, a)
	m := next(t, msgs)
	if m.Kind != protocol.KindApprovalReply || m.ApprovalID != "c1" || m.Verdict != "n" {
		t.Fatalf("got %s %q %q, want the latest decision c1 n", m.Kind, m.ApprovalID, m.Verdict)
	}
	nextIsState(t, portal, msgs)

	// Still unsettled, so the next session gets it again.
	_, again := openConsole(t, a)
	if m := next(t, again); m.Kind != protocol.KindApprovalReply || m.ApprovalID != "c1" {
		t.Fatalf("second session: got %s %q", m.Kind, m.ApprovalID)
	}
}

func TestPhoneDoneCardSettlesOutbox(t *testing.T) {
	a, _, app := newPhoneAgent(t)
	io.WriteString(app, `{"id":"c1","verdict":"y"}`+"\n")
	waitOutbox(t, a.phone, "c1", "y")

	resp := a.handleConsoleRPC(protocol.Message{
		Kind: protocol.KindApprovalPush,
		Data: cardData(t, protocol.ApprovalCard{ID: "c1", State: protocol.ApprovalDone, Result: protocol.ResultAllowed}),
	})
	if resp.Kind != protocol.KindApprovalPush {
		t.Fatalf("done push: %s %q", resp.Kind, resp.Reason)
	}
	portal, msgs := openConsole(t, a)
	nextIsState(t, portal, msgs)
}

func TestPhoneOutboxBounded(t *testing.T) {
	p := newApprovalPhone(io.Discard, nil, nil)
	now := time.Unix(1_800_000_000, 0)
	p.now = func() time.Time { return now }
	for i := 0; i <= maxOutbox; i++ {
		now = now.Add(time.Second)
		p.decide(phoneDecision{id: "c" + strconv.Itoa(i), verdict: "y"})
	}
	if len(p.outbox) != maxOutbox {
		t.Fatalf("outbox holds %d, want %d", len(p.outbox), maxOutbox)
	}
	if _, ok := p.outbox["c0"]; ok {
		t.Fatal("the oldest decision should have made room for the newest")
	}

	now = now.Add(outboxTTL)
	var resent int
	p.attach(func(protocol.Message) error { resent++; return nil })()
	if resent != 0 || len(p.outbox) != 0 {
		t.Fatalf("decisions older than %s: resent %d, kept %d", outboxTTL, resent, len(p.outbox))
	}
}

// countingApprover denies everything and counts how often it was asked.
type countingApprover struct{ n atomic.Int32 }

func (c *countingApprover) Ask(policy.Request) policy.Decision {
	c.n.Add(1)
	return policy.Decision{Allow: false}
}

func testFP(b byte) string {
	return "SHA256:" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32))
}

func newGrantAgent(t *testing.T) (*Agent, *countingApprover) {
	t.Helper()
	a := newOptsAgent(t, Options{})
	appr := &countingApprover{}
	a.setApprover(appr)
	return a, appr
}

func grantOnce(a *Agent, kind policy.Kind, pattern, fp string, sec int) protocol.Message {
	return a.handleConsoleRPC(protocol.Message{
		Kind: protocol.KindGrantOnce, RuleKind: string(kind), Pattern: pattern, FP: fp,
		TimeoutSec: sec, Approver: "phone:PGBM10",
	})
}

func mustGrant(t *testing.T, a *Agent, kind policy.Kind, pattern, fp string, sec int) {
	t.Helper()
	if resp := grantOnce(a, kind, pattern, fp, sec); resp.Kind != protocol.KindGrantOnce {
		t.Fatalf("grant_once %s %q: %s %q", kind, pattern, resp.Kind, resp.Reason)
	}
}

func execReq(cmd, fp string) policy.Request {
	return policy.Request{Kind: policy.KindExec, Cmd: cmd, Peer: fp}
}

func TestLateGrantReleasesOnce(t *testing.T) {
	a, appr := newGrantAgent(t)
	ctrl := testFP(1)
	mustGrant(t, a, policy.KindExec, "make deploy", ctrl, 1800)

	if ok, decision := a.gate(execReq("make deploy", ctrl)); !ok || decision != "late-approved" {
		t.Fatalf("first retry: %v %q", ok, decision)
	}
	if n := appr.n.Load(); n != 0 {
		t.Fatalf("the owner was asked %d times about an approved retry", n)
	}
	if ok, decision := a.gate(execReq("make deploy", ctrl)); ok || decision != "denied" {
		t.Fatalf("second request: %v %q, want asked and denied", ok, decision)
	}
	if n := appr.n.Load(); n != 1 {
		t.Fatalf("second request asked %d times, want 1", n)
	}

	events, err := a.log.Read(eventlog.Filter{Grep: "late approval"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].PeerFP != ctrl ||
		events[0].Detail != "late approval exec make deploy by phone:PGBM10, once within 30m" {
		t.Fatalf("install not recorded as expected: %+v", events)
	}
}

func TestLateGrantMatchesOnlyItsRequest(t *testing.T) {
	a, appr := newGrantAgent(t)
	ctrl, other := testFP(1), testFP(2)
	mustGrant(t, a, policy.KindExec, "make deploy", ctrl, 1800)

	for name, req := range map[string]policy.Request{
		"another controller":     execReq("make deploy", other),
		"another command":        execReq("make deploy; rm -rf ~", ctrl),
		"the same, but elevated": {Kind: policy.KindExecElevated, Cmd: "make deploy", Peer: ctrl},
	} {
		if ok, decision := a.gate(req); ok || decision != "denied" {
			t.Errorf("%s: %v %q, want asked and denied", name, ok, decision)
		}
	}
	if n := appr.n.Load(); n != 3 {
		t.Fatalf("asked %d times, want 3", n)
	}
	// None of them spent it.
	if ok, decision := a.gate(execReq("make deploy", ctrl)); !ok || decision != "late-approved" {
		t.Fatalf("the grant's own request: %v %q", ok, decision)
	}
}

func TestLateGrantElevated(t *testing.T) {
	a, _ := newGrantAgent(t)
	ctrl := testFP(1)
	mustGrant(t, a, policy.KindExecElevated, "pm list packages", ctrl, 1800)
	if ok, _ := a.gate(execReq("pm list packages", ctrl)); ok {
		t.Fatal("an elevated grant released the unelevated command")
	}
	if ok, decision := a.gate(policy.Request{Kind: policy.KindExecElevated, Cmd: "pm list packages", Peer: ctrl}); !ok || decision != "late-approved" {
		t.Fatalf("elevated retry: %v %q", ok, decision)
	}
}

func TestLateGrantExpires(t *testing.T) {
	a, _ := newGrantAgent(t)
	ctrl := testFP(1)
	now := time.Unix(1_800_000_000, 0)
	a.grants.now = func() time.Time { return now }

	mustGrant(t, a, policy.KindExec, "short", ctrl, 60)
	mustGrant(t, a, policy.KindExec, "default", ctrl, 0)
	mustGrant(t, a, policy.KindExec, "clamped", ctrl, 1<<40)

	now = now.Add(61 * time.Second)
	if ok, _ := a.gate(execReq("short", ctrl)); ok {
		t.Fatal("a 60 s grant released a request 61 s later")
	}
	now = now.Add(protocol.LateGrantWindow - 62*time.Second)
	for _, cmd := range []string{"default", "clamped"} {
		if a.grants.find(execReq(cmd, ctrl)) == nil {
			t.Fatalf("%s: gone before %s", cmd, protocol.LateGrantWindow)
		}
	}
	now = now.Add(2 * time.Second)
	for _, cmd := range []string{"default", "clamped"} {
		if ok, _ := a.gate(execReq(cmd, ctrl)); ok {
			t.Fatalf("%s: still released after %s", cmd, protocol.LateGrantWindow)
		}
	}
}

// A script's card shows an abbreviated token. The grant must carry the whole
// one: matching on the abbreviation would let a second script with the same
// visible prefix ride the first one's approval (internal/script, Canonical).
func TestLateGrantScriptBoundToWholeToken(t *testing.T) {
	a, appr := newGrantAgent(t)
	ctrl := testFP(1)
	cmd, err := script.Command(script.POSIX, []byte("make deploy\n"))
	if err != nil {
		t.Fatal(err)
	}
	label, token := policy.CommandLabel(cmd), policy.CommandPattern(cmd)
	if label == token {
		t.Fatalf("test needs an abbreviated label, got %q", label)
	}

	// The device asks, nobody answers in time, the owner approves the card.
	if ok, _ := a.gate(execReq(cmd, ctrl)); ok {
		t.Fatal("first request should have been denied")
	}
	mustGrant(t, a, policy.KindExec, label, ctrl, 1800)
	if len(a.grants.grants) != 1 || a.grants.grants[0].key != token {
		t.Fatalf("grant not bound to the whole token: %+v", a.grants.grants)
	}
	if ok, decision := a.gate(execReq(cmd, ctrl)); !ok || decision != "late-approved" {
		t.Fatalf("retry: %v %q", ok, decision)
	}
	if n := appr.n.Load(); n != 1 {
		t.Fatalf("asked %d times, want once (before the grant)", n)
	}
}

// A script label the device never asked about (it restarted since, say) is
// taken literally, and a literal abbreviation matches no script.
func TestLateGrantUnaskedScriptAsksAgain(t *testing.T) {
	a, appr := newGrantAgent(t)
	ctrl := testFP(1)
	cmd, err := script.Command(script.POSIX, []byte("id\n"))
	if err != nil {
		t.Fatal(err)
	}
	mustGrant(t, a, policy.KindExec, policy.CommandLabel(cmd), ctrl, 1800)
	if ok, _ := a.gate(execReq(cmd, ctrl)); ok || appr.n.Load() != 1 {
		t.Fatalf("released a script by its abbreviation (ok=%v, asked %d)", ok, appr.n.Load())
	}
}

// Two different scripts behind one label from the same controller is what a
// prefix collision looks like. Neither gets the approval.
func TestLateGrantAmbiguousScriptRefused(t *testing.T) {
	a, _ := newGrantAgent(t)
	ctrl := testFP(1)
	now := time.Now()
	label := "script:sh:0123456789abcdef…"
	a.grants.scripts = []askedScript{
		{kind: policy.KindExec, fp: ctrl, label: label, token: "script:sh:0123456789abcdef" + strings.Repeat("a", 48), at: now},
		{kind: policy.KindExec, fp: ctrl, label: label, token: "script:sh:0123456789abcdef" + strings.Repeat("b", 48), at: now},
	}
	resp := grantOnce(a, policy.KindExec, label, ctrl, 1800)
	if resp.Kind != protocol.KindError || !strings.Contains(resp.Reason, "more than one script") {
		t.Fatalf("ambiguous label: %s %q", resp.Kind, resp.Reason)
	}
	if len(a.grants.grants) != 0 {
		t.Fatal("a refused grant was installed")
	}
}

func TestLateGrantFileByPath(t *testing.T) {
	a, appr := newGrantAgent(t)
	ctrl := testFP(1)
	path := "/srv/app/config.yml"
	mustGrant(t, a, policy.KindWrite, path, ctrl, 1800)

	if ok, _, _ := a.gateFile(policy.Request{Kind: policy.KindRead, Path: path, Peer: ctrl}); ok {
		t.Fatal("a write grant released a read")
	}
	if ok, _, _ := a.gateFile(policy.Request{Kind: policy.KindWrite, Path: "/srv/app/other.yml", Peer: ctrl}); ok {
		t.Fatal("a grant released another path")
	}
	ok, decision, root := a.gateFile(policy.Request{Kind: policy.KindWrite, Path: path, Peer: ctrl})
	if !ok || decision != "late-approved" || root != "/srv/app" {
		t.Fatalf("retry: %v %q root %q", ok, decision, root)
	}
	if ok, _, _ := a.gateFile(policy.Request{Kind: policy.KindWrite, Path: path, Peer: ctrl}); ok {
		t.Fatal("released twice")
	}
	if n := appr.n.Load(); n != 3 {
		t.Fatalf("asked %d times, want 3", n)
	}
}

func TestLateGrantLogs(t *testing.T) {
	a, _ := newGrantAgent(t)
	ctrl := testFP(1)
	mustGrant(t, a, policy.KindLogs, "", ctrl, 1800)
	if ok, decision := a.gateDataCapability(capabilityReadEventLog, ctrl); !ok || decision != "late-approved" {
		t.Fatalf("logs retry: %v %q", ok, decision)
	}
}

func TestGrantOnceRejectsInvalid(t *testing.T) {
	a, _ := newGrantAgent(t)
	ctrl := testFP(1)
	for name, msg := range map[string]protocol.Message{
		"unknown kind":     {RuleKind: "shell", Pattern: "id", FP: ctrl, Approver: "phone:x"},
		"no kind":          {Pattern: "id", FP: ctrl, Approver: "phone:x"},
		"bad fingerprint":  {RuleKind: "exec", Pattern: "id", FP: "SHA256:short", Approver: "phone:x"},
		"no fingerprint":   {RuleKind: "exec", Pattern: "id", Approver: "phone:x"},
		"no command":       {RuleKind: "exec", FP: ctrl, Approver: "phone:x"},
		"no path":          {RuleKind: "write", FP: ctrl, Approver: "phone:x"},
		"logs with a path": {RuleKind: "logs", Pattern: "/etc", FP: ctrl, Approver: "phone:x"},
		"no approver":      {RuleKind: "exec", Pattern: "id", FP: ctrl},
	} {
		msg.Kind = protocol.KindGrantOnce
		if resp := a.handleConsoleRPC(msg); resp.Kind != protocol.KindError || resp.Reason == "" {
			t.Errorf("%s: accepted (%s)", name, resp.Kind)
		}
	}
	if len(a.grants.grants) != 0 {
		t.Fatalf("invalid grants installed: %+v", a.grants.grants)
	}
}

// Rules and bypass keep precedence, and a grant is spent only on a request
// that may otherwise proceed.
func TestLateGrantPrecedenceAndChecks(t *testing.T) {
	a, _ := newGrantAgent(t)
	ctrl := testFP(1)
	mustGrant(t, a, policy.KindExec, "uptime", ctrl, 1800)

	if ok, decision := a.gate(execReq("uptime", ctrl), func() bool { return false }); ok || decision != "delegation inactive" {
		t.Fatalf("inactive delegation: %v %q", ok, decision)
	}
	if err := a.engine.Add(policy.Rule{Kind: policy.KindExec, Pattern: "uptime", Scope: policy.ScopeGlobal}); err != nil {
		t.Fatal(err)
	}
	if ok, decision := a.gate(execReq("uptime", ctrl)); !ok || decision != "pre-approved" {
		t.Fatalf("with a rule: %v %q", ok, decision)
	}
	if len(a.grants.grants) != 1 {
		t.Fatal("the grant was spent although a rule or a failed check decided")
	}
}
