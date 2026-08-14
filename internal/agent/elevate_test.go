package agent

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"wanctl/internal/elevate"
	"wanctl/internal/eventlog"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
	"wanctl/internal/relay"
	"wanctl/internal/transport"
)

// execElevated is execOnce with the elevation fields set, and it returns the
// channel the device claims ran the command.
func execElevated(t *testing.T, dr *transport.DialResult, cmd, via string) (out string, code int, reason, ranVia string) {
	t.Helper()
	protocol.WriteMessage(dr.Conn, protocol.Message{
		Kind: protocol.KindExec, Command: cmd, OneShot: true, Elevate: true, Via: via,
	})
	var b strings.Builder
	for {
		ft, p, err := protocol.ReadFrame(dr.Conn)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		switch ft {
		case protocol.FrameStdout:
			b.Write(p)
		case protocol.FrameJSON:
			m, _ := protocol.DecodeMessage(p)
			switch m.Kind {
			case protocol.KindExit:
				return b.String(), m.Code, "", m.ElevatedVia
			case protocol.KindReject, protocol.KindError:
				return b.String(), -1, m.Reason, ""
			}
		}
	}
}

func relayBase(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// fakeChannel is an elevation channel that always works, so these tests are
// about the gate and the wire rather than about su.
type fakeChannel struct {
	kind elevate.Kind
	ran  []string
}

func (f *fakeChannel) Kind() elevate.Kind { return f.kind }
func (f *fakeChannel) Probe(context.Context) elevate.Status {
	return elevate.Status{Available: true, Detail: "uid=0(root)"}
}
func (f *fakeChannel) Run(_ context.Context, command, _ string, out io.Writer) (int, error) {
	f.ran = append(f.ran, command)
	io.WriteString(out, "uid=0(root)\n")
	return 0, nil
}
func (f *fakeChannel) Close() error { return nil }

// TestBypassDoesNotAuthorizeElevatedCommands is the acceptance test for the
// owner's decision on 2026-08-14: a device left in bypass so it can be used
// unattended must still refuse to run a command as root until someone says so.
// The plain command in the same session proves bypass is otherwise working.
func TestBypassDoesNotAuthorizeElevatedCommands(t *testing.T) {
	base := relayBase(t)
	ag := startAgent(t, base, policy.DenyApprover{}, policy.ModeBypass)
	ag.elevator = elevate.NewManager(true, "", &fakeChannel{kind: elevate.KindSu})
	dr := connectController(t, base)
	defer dr.Conn.Close()

	if out, code, reason := execOnce(t, dr, "echo plain-ok", ""); code != 0 || !strings.Contains(out, "plain-ok") {
		t.Fatalf("bypass stopped covering ordinary commands: out=%q code=%d reason=%q", out, code, reason)
	}

	_, code, reason, _ := execElevated(t, dr, "id", "")
	if code != -1 {
		t.Fatalf("bypass mode ran an elevated command (code=%d); root must need its own approval", code)
	}
	if !strings.Contains(reason, "elevated command denied") {
		t.Fatalf("rejection = %q, want it to name the elevated class", reason)
	}
	if !strings.Contains(reason, "bypass mode does not cover them") {
		t.Fatalf("rejection = %q, want it to explain why bypass did not help", reason)
	}
}

// TestElevatedCommandRunsAndReportsItsChannel is the positive path: approved,
// executed on a channel, and the exit message names which one.
func TestElevatedCommandRunsAndReportsItsChannel(t *testing.T) {
	base := relayBase(t)
	ag := startAgent(t, base, policy.AllowApprover{}, policy.ModeNormal)
	ch := &fakeChannel{kind: elevate.KindSu}
	ag.elevator = elevate.NewManager(true, "", ch)
	dr := connectController(t, base)
	defer dr.Conn.Close()

	out, code, reason, ranVia := execElevated(t, dr, "id", "")
	if code != 0 || reason != "" {
		t.Fatalf("elevated exec failed: code=%d reason=%q", code, reason)
	}
	if !strings.Contains(out, "uid=0") {
		t.Fatalf("output = %q, want the channel's output", out)
	}
	if ranVia != string(elevate.KindSu) {
		t.Fatalf("ElevatedVia = %q, want %q — the controller needs this to tell "+
			"an elevated run from an unelevated one", ranVia, elevate.KindSu)
	}
	if len(ch.ran) != 1 || ch.ran[0] != "id" {
		t.Fatalf("channel ran %v, want [id]", ch.ran)
	}
}

// TestElevatedExecIsGatedAsItsOwnKind pins that the approver is asked about an
// elevated command, not an ordinary one — the prompt a human sees has to say
// which of the two it is.
func TestElevatedExecIsGatedAsItsOwnKind(t *testing.T) {
	base := relayBase(t)
	ap := requestKindApprover{kinds: make(chan policy.Kind, 1)}
	ag := startAgent(t, base, ap, policy.ModeNormal)
	ag.elevator = elevate.NewManager(true, "", &fakeChannel{kind: elevate.KindSu})
	dr := connectController(t, base)
	defer dr.Conn.Close()

	execElevated(t, dr, "id", "")
	select {
	case kind := <-ap.kinds:
		if kind != policy.KindExecElevated {
			t.Fatalf("elevated exec gated as %q, want %q", kind, policy.KindExecElevated)
		}
	default:
		t.Fatal("elevated exec did not enter the policy gate")
	}
}

// TestElevationOffIsRefusedBeforeItRunsUnprivileged is the failure mode that
// matters most: a device with elevation switched off must refuse, never fall
// back to running the command in the app sandbox.
func TestElevationOffIsRefusedBeforeItRunsUnprivileged(t *testing.T) {
	base := relayBase(t)
	ag := startAgent(t, base, policy.AllowApprover{}, policy.ModeNormal)
	ag.elevator = elevate.NewManager(false, "turn on 提权通道 in the wanctl app")
	dr := connectController(t, base)
	defer dr.Conn.Close()

	out, code, reason, ranVia := execElevated(t, dr, "id", "")
	if code != -1 {
		t.Fatalf("agent ran an elevated command with elevation off (code=%d, out=%q)", code, out)
	}
	if ranVia != "" {
		t.Fatalf("ElevatedVia = %q on a refusal", ranVia)
	}
	if !strings.Contains(reason, "提权通道") {
		t.Fatalf("reason = %q, want it to say how to switch elevation on", reason)
	}
}

// TestUnknownViaIsRejectedBeforeTheApprovalPrompt: nobody should be asked to
// approve a command that cannot run in any case.
func TestUnknownViaIsRejectedBeforeTheApprovalPrompt(t *testing.T) {
	base := relayBase(t)
	ap := requestKindApprover{kinds: make(chan policy.Kind, 1)}
	ag := startAgent(t, base, ap, policy.ModeNormal)
	ag.elevator = elevate.NewManager(true, "", &fakeChannel{kind: elevate.KindSu})
	dr := connectController(t, base)
	defer dr.Conn.Close()

	_, code, reason, _ := execElevated(t, dr, "id", "magisk")
	if code != -1 {
		t.Fatalf("unknown --via ran anyway (code=%d)", code)
	}
	if !strings.Contains(reason, "unknown elevation channel") {
		t.Fatalf("reason = %q, want it to name the bad channel", reason)
	}
	select {
	case kind := <-ap.kinds:
		t.Fatalf("a human was asked to approve (%s) a command with an invalid channel", kind)
	default:
	}
}

// TestPinnedUnavailableChannelDoesNotFallBack: --via su on a device where su is
// missing must fail, not quietly run through another channel.
func TestPinnedUnavailableChannelDoesNotFallBack(t *testing.T) {
	base := relayBase(t)
	ag := startAgent(t, base, policy.AllowApprover{}, policy.ModeNormal)
	shizuku := &fakeChannel{kind: elevate.KindShizuku}
	ag.elevator = elevate.NewManager(true, "", unavailable{elevate.KindSu, "no su binary found"}, shizuku)
	dr := connectController(t, base)
	defer dr.Conn.Close()

	_, code, reason, _ := execElevated(t, dr, "id", "su")
	if code != -1 {
		t.Fatalf("pinned-but-unavailable channel ran anyway (code=%d)", code)
	}
	if !strings.Contains(reason, "no su binary found") {
		t.Fatalf("reason = %q, want the channel's own explanation", reason)
	}
	if len(shizuku.ran) != 0 {
		t.Fatal("a command pinned to su ran through shizuku instead")
	}
}

type unavailable struct {
	kind   elevate.Kind
	reason string
}

func (u unavailable) Kind() elevate.Kind { return u.kind }
func (u unavailable) Probe(context.Context) elevate.Status {
	return elevate.Status{Available: false, Reason: u.reason}
}
func (u unavailable) Run(context.Context, string, string, io.Writer) (int, error) {
	return -1, io.EOF // never reached
}
func (u unavailable) Close() error { return nil }

// TestElevatedRunIsAudited: "what has run as root on this phone" must be a
// question the event log can answer.
func TestElevatedRunIsAudited(t *testing.T) {
	base := relayBase(t)
	ag := startAgent(t, base, policy.AllowApprover{}, policy.ModeNormal)
	ag.elevator = elevate.NewManager(true, "", &fakeChannel{kind: elevate.KindSu})
	dr := connectController(t, base)
	defer dr.Conn.Close()

	execOnce(t, dr, "echo plain", "")
	execElevated(t, dr, "id", "")

	events, err := ag.log.Read(eventlog.Filter{Type: "exec"})
	if err != nil {
		t.Fatal(err)
	}
	var elevated, plain int
	for _, e := range events {
		if e.Type != "exec" {
			continue
		}
		switch e.Via {
		case string(elevate.KindSu):
			elevated++
			if e.Detail != "id" {
				t.Fatalf("elevated event detail = %q, want the command", e.Detail)
			}
		case "":
			plain++
		default:
			t.Fatalf("unexpected via %q", e.Via)
		}
	}
	if elevated != 1 {
		t.Fatalf("event log recorded %d elevated execs, want 1", elevated)
	}
	if plain == 0 {
		t.Fatal("event log lost the ordinary exec")
	}
}
