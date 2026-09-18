package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"wanctl/internal/elevate"
	"wanctl/internal/eventlog"
	"wanctl/internal/policy"
	"wanctl/internal/script"
)

// The elevation gate after the owner's decision of 2026-09-18 (issue #71).
//
// Two switches, both off by default, are the consent: 自动放行所有命令 (bypass
// mode) and 提权通道 (the elevation channel). A device with both on runs an
// elevated command the way it runs any other, logged as `bypass` and carrying
// the channel that ran it. A device with only one of them still refuses.

// recordingApprover is the approver seam: it answers with a fixed decision and
// keeps every request it was asked about, so a test can assert both that the
// owner was reached and that they were reached once.
type recordingApprover struct {
	mu   sync.Mutex
	got  []policy.Request
	give policy.Decision
}

func (a *recordingApprover) Ask(req policy.Request) policy.Decision {
	a.mu.Lock()
	a.got = append(a.got, req)
	a.mu.Unlock()
	return a.give
}

func (a *recordingApprover) asked() []policy.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]policy.Request(nil), a.got...)
}

// lastExec returns the newest exec event, which is where the decision the gate
// reached is recorded.
func lastExec(t *testing.T, ag *Agent) eventlog.Event {
	t.Helper()
	events, err := ag.log.Read(eventlog.Filter{Type: "exec"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("no exec event was recorded")
	}
	return events[len(events)-1]
}

func TestBypassWithElevationChannelRunsElevatedCommands(t *testing.T) {
	base := relayBase(t)
	// DenyApprover: if this passes it is because bypass covered it, not because
	// someone was asked and said yes.
	ag := startAgent(t, base, policy.DenyApprover{}, policy.ModeBypass)
	ch := &fakeChannel{kind: elevate.KindSu}
	ag.elevator = elevate.NewManager(true, "", ch)
	dr := connectController(t, base)
	defer dr.Conn.Close()

	out, code, reason, ranVia := execElevated(t, dr, "id", "")
	if code != 0 || reason != "" {
		t.Fatalf("bypass + elevation channel on refused an elevated command: code=%d reason=%q", code, reason)
	}
	if !strings.Contains(out, "uid=0") {
		t.Fatalf("output = %q, want the channel's output", out)
	}
	if ranVia != string(elevate.KindSu) {
		t.Fatalf("ElevatedVia = %q, want %q", ranVia, elevate.KindSu)
	}
	ev := lastExec(t, ag)
	if ev.Decision != "bypass" {
		t.Fatalf("event decision = %q, want %q — an auto-allowed elevated command "+
			"must be readable as one in the audit log", ev.Decision, "bypass")
	}
	if ev.Via != string(elevate.KindSu) {
		t.Fatalf("event via = %q, want the channel that ran it", ev.Via)
	}
}

func TestBypassWithoutElevationChannelStillRefuses(t *testing.T) {
	base := relayBase(t)
	ag := startAgent(t, base, policy.DenyApprover{}, policy.ModeBypass)
	// Elevation switched off: the channel exists in the binary, the owner has
	// not turned it on.
	ag.elevator = elevate.NewManager(false, "turn on 提权通道 in the wanctl app", &fakeChannel{kind: elevate.KindSu})
	dr := connectController(t, base)
	defer dr.Conn.Close()

	// The plain command proves bypass is otherwise working, so the refusal
	// below is about the elevated class and not about a broken session.
	if out, code, reason := execOnce(t, dr, "echo plain-ok", ""); code != 0 || !strings.Contains(out, "plain-ok") {
		t.Fatalf("bypass stopped covering ordinary commands: out=%q code=%d reason=%q", out, code, reason)
	}

	_, code, reason, _ := execElevated(t, dr, "id", "")
	if code != -1 {
		t.Fatalf("one switch was enough to run a command as root (code=%d)", code)
	}
	if !strings.Contains(reason, "elevated command denied by device policy") {
		t.Fatalf("reason = %q, want the existing elevated-denial message", reason)
	}
	if !strings.Contains(reason, "bypass mode does not cover them") {
		t.Fatalf("reason = %q, want it to explain why bypass did not help", reason)
	}
}

// An elevated rule miss on a normal-mode device reaches whoever approves — the
// portal device console, the device's own prompt, a notification — instead of
// being refused where nobody can see it (#71 §1).
func TestNormalModeElevatedMissReachesTheApprover(t *testing.T) {
	base := relayBase(t)
	ap := &recordingApprover{give: policy.Decision{Allow: true}}
	ag := startAgent(t, base, ap, policy.ModeNormal)
	ag.elevator = elevate.NewManager(true, "", &fakeChannel{kind: elevate.KindSu})
	dr := connectController(t, base)
	defer dr.Conn.Close()

	_, code, reason, _ := execElevated(t, dr, "id", "")
	if code != 0 {
		t.Fatalf("approved elevated command did not run: code=%d reason=%q", code, reason)
	}
	asked := ap.asked()
	if len(asked) != 1 {
		t.Fatalf("approver was asked %d times, want 1", len(asked))
	}
	if asked[0].Kind != policy.KindExecElevated {
		t.Fatalf("asked about %q, want %q — the prompt has to say which of the two this is", asked[0].Kind, policy.KindExecElevated)
	}
	if asked[0].Cmd != "id" {
		t.Fatalf("asked about %q, want the command", asked[0].Cmd)
	}
	if ev := lastExec(t, ag); ev.Decision != "approved" {
		t.Fatalf("event decision = %q, want %q", ev.Decision, "approved")
	}
}

func TestNormalModeElevatedDenialIsRefused(t *testing.T) {
	base := relayBase(t)
	ap := &recordingApprover{give: policy.Decision{Allow: false}}
	ag := startAgent(t, base, ap, policy.ModeNormal)
	ch := &fakeChannel{kind: elevate.KindSu}
	ag.elevator = elevate.NewManager(true, "", ch)
	dr := connectController(t, base)
	defer dr.Conn.Close()

	_, code, reason, _ := execElevated(t, dr, "id", "")
	if code != -1 {
		t.Fatalf("a refused elevated command ran anyway (code=%d)", code)
	}
	if !strings.Contains(reason, "elevated command denied") {
		t.Fatalf("reason = %q", reason)
	}
	if len(ch.ran) != 0 {
		t.Fatalf("the channel ran %v after a refusal", ch.ran)
	}
	if ev := lastExec(t, ag); ev.Decision != "denied" {
		t.Fatalf("event decision = %q, want %q", ev.Decision, "denied")
	}
}

// "Allow + remember" from an elevated approval writes an exec-elevated rule, so
// the second identical command runs without asking anyone again.
func TestElevatedApproveAndRememberCreatesAnElevatedRule(t *testing.T) {
	base := relayBase(t)
	ap := &recordingApprover{give: policy.Decision{Allow: true, Remember: true, Scope: policy.ScopeGlobal}}
	ag := startAgent(t, base, ap, policy.ModeNormal)
	ag.elevator = elevate.NewManager(true, "", &fakeChannel{kind: elevate.KindSu})
	dr := connectController(t, base)
	defer dr.Conn.Close()

	if _, code, reason, _ := execElevated(t, dr, "pm list packages", ""); code != 0 {
		t.Fatalf("first elevated command failed: code=%d reason=%q", code, reason)
	}
	rules := ag.engine.List()
	if len(rules) != 1 || rules[0].Kind != policy.KindExecElevated || rules[0].Pattern != "pm list packages" {
		t.Fatalf("remembered rules = %+v, want one exec-elevated rule for the command", rules)
	}

	if _, code, reason, _ := execElevated(t, dr, "pm list packages", ""); code != 0 {
		t.Fatalf("second elevated command failed: code=%d reason=%q", code, reason)
	}
	if n := len(ap.asked()); n != 1 {
		t.Fatalf("approver was asked %d times, want 1 — the remembered rule did not take", n)
	}
	if ev := lastExec(t, ag); ev.Decision != "pre-approved" {
		t.Fatalf("second run's decision = %q, want %q", ev.Decision, "pre-approved")
	}

	// The remembered grant is the elevated one only: the same command
	// unelevated is still a separate decision. (Asked of the engine rather than
	// over the wire, because this test's approver says yes to everything.)
	if ag.engine.Allowed(policy.Request{Kind: policy.KindExec, Cmd: "pm list packages"}) {
		t.Fatal("the remembered elevated rule also authorized the unelevated command")
	}
}

// A scripted elevated command is remembered by the script's token, not by the
// base64 transport blob, and the token still matches the same script (#63).
func TestElevatedScriptIsRememberedByItsToken(t *testing.T) {
	base := relayBase(t)
	ap := &recordingApprover{give: policy.Decision{Allow: true, Remember: true, Scope: policy.ScopeGlobal}}
	ag := startAgent(t, base, ap, policy.ModeNormal)
	ag.elevator = elevate.NewManager(true, "", &fakeChannel{kind: elevate.KindSu})
	dr := connectController(t, base)
	defer dr.Conn.Close()

	cmd, err := script.Command(script.POSIX, []byte("pm install -r /sdcard/Download/app.apk\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, code, reason, _ := execElevated(t, dr, cmd, ""); code != 0 {
		t.Fatalf("scripted elevated command failed: code=%d reason=%q", code, reason)
	}
	rules := ag.engine.List()
	if len(rules) != 1 {
		t.Fatalf("remembered %d rules, want 1: %+v", len(rules), rules)
	}
	if !strings.HasPrefix(rules[0].Pattern, script.CanonicalPrefix) {
		t.Fatalf("remembered pattern = %q, want the script token", rules[0].Pattern)
	}
	if _, code, reason, _ := execElevated(t, dr, cmd, ""); code != 0 {
		t.Fatalf("the remembered script rule did not match a re-run: code=%d reason=%q", code, reason)
	}
	if n := len(ap.asked()); n != 1 {
		t.Fatalf("approver was asked %d times, want 1", n)
	}
}

// Whether a verb name without --elevate is answered with "add --elevate" is a
// per-platform decision, unit-tested in internal/androidverb. What this test
// pins is the desktop half of it end to end: a real program named `app` on the
// device's PATH runs, exactly as it did before the verb check existed. A
// desktop has no elevation channel to switch on, so answering it with advice
// about --elevate would be both wrong and unfixable (review of #108).
func TestDesktopRunsAProgramNamedLikeAVerb(t *testing.T) {
	if runtime.GOOS == "android" || runtime.GOOS == "windows" {
		t.Skip("posix desktop shells only")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app"), []byte("#!/bin/sh\nprintf desktop-app-ok\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	base := relayBase(t)
	startAgent(t, base, policy.AllowApprover{}, policy.ModeBypass)
	dr := connectController(t, base)
	defer dr.Conn.Close()

	out, code, reason := execOnce(t, dr, "app install whatever", "")
	if strings.Contains(reason, "--elevate") {
		t.Fatalf("a desktop program named like an Android verb was refused: %q", reason)
	}
	if code != 0 || !strings.Contains(out, "desktop-app-ok") {
		t.Fatalf("the program did not run: out=%q code=%d reason=%q", out, code, reason)
	}
}

// Nothing a controller can put in a command may kill the agent. A command
// shaped like the --script transport but too short to be one panicked inside
// the text of its own REFUSAL: no rule, no approval, no elevation needed —
// a paired controller on a normal-mode device took the whole agent down
// (review of #108, P1).
func TestMalformedScriptShapedCommandIsRefusedNotFatal(t *testing.T) {
	base := relayBase(t)
	ag := startAgent(t, base, policy.DenyApprover{}, policy.ModeNormal)
	ag.elevator = elevate.NewManager(true, "", &fakeChannel{kind: elevate.KindSu})
	dr := connectController(t, base)
	defer dr.Conn.Close()

	for _, cmd := range []string{
		"printf %s ' | base64 -d | sh",
		"powershell -NoProfile -NonInteractive -EncodedCommand '",
	} {
		if _, code, reason, _ := execElevated(t, dr, cmd, ""); code != -1 || reason == "" {
			t.Fatalf("%q: code=%d reason=%q, want a refusal", cmd, code, reason)
		}
		// Still serving: it answers the next request on the same session. (A
		// denial, because this device's approver says no to everything — what
		// matters is that something came back at all.)
		if _, _, reason := execOnce(t, dr, "echo still-here", ""); !strings.Contains(reason, "echo still-here") {
			t.Fatalf("after refusing %q the agent stopped answering: reason=%q", cmd, reason)
		}
	}
}

// The same command on the approval path: it reaches the approver and is drawn
// on a card, both of which render it.
func TestMalformedScriptShapedCommandReachesTheApprover(t *testing.T) {
	base := relayBase(t)
	ap := &recordingApprover{give: policy.Decision{Allow: false}}
	ag := startAgent(t, base, ap, policy.ModeNormal)
	ag.elevator = elevate.NewManager(true, "", &fakeChannel{kind: elevate.KindSu})
	dr := connectController(t, base)
	defer dr.Conn.Close()

	const cmd = "printf %s ' | base64 -d | sh"
	execElevated(t, dr, cmd, "")
	asked := ap.asked()
	if len(asked) != 1 || asked[0].Cmd != cmd {
		t.Fatalf("approver saw %+v, want one request for the command itself", asked)
	}
	if got := policy.CommandLabel(cmd); got != cmd {
		t.Fatalf("the card would draw %q, want the command unchanged — it is not a script", got)
	}
}

// The console queue is the portal's side of an approval, and it is where the
// label/pattern split has to hold: the card shows an abbreviated script token,
// the remembered rule carries the whole digest, and what is on the card is a
// visible prefix of what was written so a person can check one against the
// other. Nothing ever matches on the short form.
func TestConsoleCardAbbreviatesWhatTheRuleStoresWhole(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	ag, err := New(Options{Name: "console-test", RelayURL: "ws://unused", Token: "unused", Mode: policy.ModeNormal, Shell: "/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ag.Close)
	changed, cancel := ag.console.Subscribe()
	defer cancel()

	cmd, err := script.Command(script.POSIX, []byte("pm install -r /sdcard/app.apk\n"))
	if err != nil {
		t.Fatal(err)
	}
	allowed := make(chan bool, 1)
	go func() {
		ok, _ := ag.gate(policy.Request{Kind: policy.KindExecElevated, Cmd: cmd, Via: "adb"})
		allowed <- ok
	}()

	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("the elevated request never reached the console queue")
	}
	pending := ag.console.State().Pending
	if len(pending) != 1 || pending[0].Kind != string(policy.KindExecElevated) {
		t.Fatalf("pending = %+v, want one exec-elevated request", pending)
	}
	card := pending[0].Cmd
	full, _ := script.Canonical(cmd)
	if card == full {
		t.Fatalf("the card drew the whole digest %q; it is a wall, not information", card)
	}
	if !strings.HasPrefix(full, strings.TrimSuffix(card, "…")) {
		t.Fatalf("card %q is not a visible prefix of the token %q", card, full)
	}

	if !ag.console.Decide(pending[0].ID, "g") {
		t.Fatal("the console could not deliver a verdict")
	}
	if !<-allowed {
		t.Fatal("an approved elevated command was refused")
	}
	rules := ag.engine.List()
	if len(rules) != 1 || rules[0].Pattern != full {
		t.Fatalf("remembered %+v, want one rule carrying the whole token %q", rules, full)
	}
	if !ag.engine.Allowed(policy.Request{Kind: policy.KindExecElevated, Cmd: cmd}) {
		t.Fatal("the remembered rule does not match the script it was written for")
	}
	if ag.engine.Allowed(policy.Request{Kind: policy.KindExecElevated, Cmd: card}) {
		t.Fatal("the abbreviation on the card matched as a command")
	}
}
