package agent

import (
	"path/filepath"
	"testing"
	"wanctl/internal/protocol"

	"wanctl/internal/policy"
)

// A late approval is consent to retry the operation on the phone card,
// including its working directory, rather than to run that command anywhere.
func TestLateApprovalBindsWorkingDirectory(t *testing.T) {
	a, appr := newGrantAgent(t)
	fp := testFP(1)
	original := policy.Request{Kind: policy.KindExec, Cmd: "make deploy", Cwd: t.TempDir(), Peer: fp}
	if ok, _ := a.gate(original); ok {
		t.Fatal("fixture unexpectedly allowed the original request")
	}
	// The phone carries the directory displayed on the original card.
	dir, err := filepath.EvalSymlinks(original.Cwd)
	if err != nil {
		t.Fatal(err)
	}
	original.Cwd = dir
	resp := a.handleConsoleRPC(protocol.Message{Kind: protocol.KindGrantOnce, RuleKind: string(original.Kind), Pattern: policy.CommandLabel(original.Cmd), FP: fp, Cwd: dir, TimeoutSec: 1800, Approver: "phone:test"})
	if resp.Kind != protocol.KindGrantOnce {
		t.Fatal(resp)
	}
	changed := original
	changed.Cwd = t.TempDir()
	allowed, decision := a.gate(changed)
	if allowed || appr.n.Load() != 2 {
		t.Fatalf("approval for cwd %q authorized cwd %q: allowed=%v decision=%q approval prompts=%d; want a fresh prompt and denial", original.Cwd, changed.Cwd, allowed, decision, appr.n.Load())
	}
	if ok, decision := a.gate(original); !ok || decision != "late-approved" {
		t.Fatalf("original retry: %v %q", ok, decision)
	}
}

func TestLateApprovalRequiresKnownDirectory(t *testing.T) {
	a, _ := newGrantAgent(t)
	resp := a.handleConsoleRPC(protocol.Message{Kind: protocol.KindGrantOnce, RuleKind: "exec", Pattern: "id", FP: testFP(1), Approver: "phone:test"})
	if resp.Kind != protocol.KindError {
		t.Fatal("accepted execution grant without a directory")
	}
}
