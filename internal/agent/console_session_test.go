package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"wanctl/internal/console"
	"wanctl/internal/eventlog"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
)

// newTestAgent returns a minimal Agent with a wired console service for unit tests.
func newTestAgent(t *testing.T) *Agent {
	t.Helper()
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	eng, err := policy.Open("rules.json", policy.ModeNormal)
	if err != nil {
		t.Fatalf("policy.Open: %v", err)
	}
	logger, err := eventlog.Open("events.jsonl")
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	svc := console.New(eng, logger, console.Info{Device: "test-dev"})
	a := &Agent{
		engine:  eng,
		console: svc,
		opts:    Options{Name: "test-dev"},
	}
	return a
}

func TestConsoleRPCState(t *testing.T) {
	a := newTestAgent(t)
	resp := a.handleConsoleRPC(protocol.Message{Kind: protocol.KindConsoleState})
	if resp.Kind != protocol.KindConsoleState {
		t.Fatalf("want kind %q, got %q", protocol.KindConsoleState, resp.Kind)
	}
	if !json.Valid(resp.Data) {
		t.Fatalf("Data is not valid JSON: %s", resp.Data)
	}
}

func TestConsoleRPCRuleAdd(t *testing.T) {
	a := newTestAgent(t)
	resp := a.handleConsoleRPC(protocol.Message{
		Kind:     protocol.KindRuleAdd,
		RuleKind: "exec",
		Pattern:  "echo",
		Scope:    "global",
	})
	if resp.Kind != protocol.KindRuleAdd {
		t.Fatalf("want kind %q, got %q", protocol.KindRuleAdd, resp.Kind)
	}
	if len(resp.Data) != 0 {
		t.Fatalf("expected no error in Data, got: %s", resp.Data)
	}
}

func TestConsoleRPCModeSet(t *testing.T) {
	a := newTestAgent(t)
	resp := a.handleConsoleRPC(protocol.Message{
		Kind:        protocol.KindModeSet,
		ConsoleMode: "bypass",
	})
	if resp.Kind != protocol.KindModeSet {
		t.Fatalf("want kind %q, got %q", protocol.KindModeSet, resp.Kind)
	}
}

func TestConsoleRPCDecide(t *testing.T) {
	a := newTestAgent(t)
	resp := a.handleConsoleRPC(protocol.Message{
		Kind:       protocol.KindDecide,
		ApprovalID: "nonexistent",
		Verdict:    "allow",
	})
	if resp.Kind != protocol.KindDecide {
		t.Fatalf("want kind %q, got %q", protocol.KindDecide, resp.Kind)
	}
	if resp.Verdict != "not-found" {
		t.Fatalf("want verdict %q, got %q", "not-found", resp.Verdict)
	}
}

// mkConsole returns a minimal console.Service for use in pump tests.
func mkConsole(t *testing.T) *console.Service {
	t.Helper()
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	eng, err := policy.Open("rules.json", policy.ModeNormal)
	if err != nil {
		t.Fatalf("policy.Open: %v", err)
	}
	logger, err := eventlog.Open("events.jsonl")
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	return console.New(eng, logger, console.Info{Device: "test-dev"})
}

func TestPumpApprovalNotifsStopsOnCancel(t *testing.T) {
	svc := mkConsole(t)
	changes, cancelSub := svc.Subscribe()
	defer cancelSub()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pumpApprovalNotifs(ctx, changes, svc, func(protocol.Message) error { return nil })
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not exit on context cancel (goroutine leak)")
	}
}
