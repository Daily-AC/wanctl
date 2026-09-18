package agent

import (
	"strings"
	"sync"
	"testing"

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

// A verb sent without --elevate used to reach the device shell and come back as
// exit 127, blaming the device for a missing flag (#71 "Minor").
func TestVerbWithoutElevateSaysSo(t *testing.T) {
	base := relayBase(t)
	startAgent(t, base, policy.AllowApprover{}, policy.ModeBypass)
	dr := connectController(t, base)
	defer dr.Conn.Close()

	out, code, reason := execOnce(t, dr, "app install /sdcard/Download/app.apk", "")
	if code == 127 {
		t.Fatal("the verb still fell through to the shell")
	}
	if !strings.Contains(reason, "--elevate") {
		t.Fatalf("reason = %q, want it to name the missing flag", reason)
	}
	if strings.Contains(out+reason, "not found") || strings.Contains(out+reason, "inaccessible") {
		t.Fatalf("the shell's own answer leaked through: out=%q reason=%q", out, reason)
	}
}

// …but a verb name that is also a real Android program keeps working
// unelevated: the check is about wanctl's own inventions, not about every word
// the verb table happens to contain.
func TestRealBinaryVerbsStillRunUnelevated(t *testing.T) {
	base := relayBase(t)
	startAgent(t, base, policy.AllowApprover{}, policy.ModeBypass)
	dr := connectController(t, base)
	defer dr.Conn.Close()

	_, _, reason := execOnce(t, dr, "logcat -d", "")
	if strings.Contains(reason, "--elevate") {
		t.Fatalf("logcat was refused as a wanctl verb: %q — it is a real program "+
			"a controller may legitimately run unelevated", reason)
	}
}
