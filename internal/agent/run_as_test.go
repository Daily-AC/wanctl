package agent

import (
	"runtime"
	"strings"
	"testing"

	"wanctl/internal/eventlog"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
)

func TestExecAsUsesOrdinaryPolicyAndRecordsIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses the non-Windows refusal after approval")
	}
	base := relayBase(t)
	ap := requestKindApprover{kinds: make(chan policy.Kind, 1)}
	ag := startAgent(t, base, ap, policy.ModeNormal)
	dr := connectController(t, base)
	defer dr.Conn.Close()
	got := fileOpReply(t, dr, protocol.Message{Kind: protocol.KindExecAs, Command: "whoami", As: "alice"})
	if got.Kind != protocol.KindError || !strings.Contains(got.Reason, "only supported by Windows") {
		t.Fatalf("reply = %+v", got)
	}
	select {
	case kind := <-ap.kinds:
		if kind != policy.KindExec {
			t.Fatalf("policy kind = %q, want ordinary exec", kind)
		}
	default:
		t.Fatal("--as bypassed policy")
	}
	events, err := ag.log.Read(eventlog.Filter{Type: "exec"})
	if err != nil || len(events) != 1 || events[0].As != "alice" {
		t.Fatalf("audit = %+v, error %v", events, err)
	}
}

func TestExecAsRejectsInvalidRequestsBeforeApproval(t *testing.T) {
	base := relayBase(t)
	ap := requestKindApprover{kinds: make(chan policy.Kind, 4)}
	startAgent(t, base, ap, policy.ModeNormal)
	dr := connectController(t, base)
	defer dr.Conn.Close()
	for _, msg := range []protocol.Message{
		{Kind: protocol.KindExecAs},
		{Kind: protocol.KindExecAs, As: "alice", Elevate: true},
		{Kind: protocol.KindExecAs, As: "alice", Via: "adb"},
		{Kind: protocol.KindExecAsync, As: "alice"},
	} {
		msg.Command = "whoami"
		if got := fileOpReply(t, dr, msg); got.Kind != protocol.KindError {
			t.Fatalf("invalid request %+v accepted: %+v", msg, got)
		}
	}
	select {
	case kind := <-ap.kinds:
		t.Fatalf("invalid request reached policy: %s", kind)
	default:
	}
}

func TestExecAsCannotBypassPolicyDenial(t *testing.T) {
	base := relayBase(t)
	startAgent(t, base, policy.DenyApprover{}, policy.ModeNormal)
	dr := connectController(t, base)
	defer dr.Conn.Close()
	got := fileOpReply(t, dr, protocol.Message{Kind: protocol.KindExecAs, As: "alice", Command: "whoami"})
	if got.Kind != protocol.KindReject || !strings.Contains(got.Reason, "denied by device policy") {
		t.Fatalf("--as bypassed denial: %+v", got)
	}
}
