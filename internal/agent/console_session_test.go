package agent

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"wanctl/internal/console"
	"wanctl/internal/eventlog"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
)

// TestServeConsoleFramedWireAndNotif is the integration test the per-task unit
// tests stepped around: it drives a real agent.serveConsole over a pipe using
// the SAME framed protocol the portal's deviceConn speaks, proving (a) the wire
// format matches end-to-end and (b) a new pending pushes the FULL console.State
// (not just the command), and (c) a remote y-decision unblocks the gate.
func TestServeConsoleFramedWireAndNotif(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	a, err := New(Options{RelayURL: "ws://x", Token: "t", Name: "dev1"})
	if err != nil {
		t.Fatal(err)
	}

	dev, portal := net.Pipe()
	defer dev.Close()
	defer portal.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.serveConsole(ctx, dev)

	// portal-side reader: split async notifs from RPC responses (like deviceConn).
	notifCh := make(chan console.State, 8)
	respCh := make(chan protocol.Message, 8)
	go func() {
		for {
			m, err := protocol.ReadMessage(portal)
			if err != nil {
				return
			}
			if m.Kind == protocol.KindApprovalNotif {
				var st console.State
				if json.Unmarshal(m.Data, &st) == nil {
					notifCh <- st
				}
				continue
			}
			respCh <- m
		}
	}()

	// 1) state RPC round-trips over the framed wire (catches the encoding mismatch).
	if err := protocol.WriteMessage(portal, protocol.Message{Kind: protocol.KindConsoleState}); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-respCh:
		if m.Kind != protocol.KindConsoleState {
			t.Fatalf("state resp kind = %s", m.Kind)
		}
		var st console.State
		if err := json.Unmarshal(m.Data, &st); err != nil {
			t.Fatalf("state data not a console.State: %v", err)
		}
		if st.Mode != policy.ModeNormal {
			t.Fatalf("mode = %s", st.Mode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no state response (framed wire mismatch?)")
	}

	// 2) a new pending pushes a FULL console.State notif (catches the cmd-only bug).
	askDone := make(chan policy.Decision, 1)
	go func() { askDone <- a.console.Ask(policy.Request{Kind: policy.KindExec, Cmd: "rm -rf /tmp"}) }()
	var id string
	select {
	case ns := <-notifCh:
		if len(ns.Pending) != 1 {
			t.Fatalf("notif Pending = %d, want 1 (full State expected, not cmd-only)", len(ns.Pending))
		}
		if ns.Pending[0].Cmd != "rm -rf /tmp" {
			t.Fatalf("notif cmd = %q", ns.Pending[0].Cmd)
		}
		id = ns.Pending[0].ID
	case <-time.After(2 * time.Second):
		t.Fatal("no approval notif pushed")
	}

	// 3) a remote y-decision unblocks the gate.
	if err := protocol.WriteMessage(portal, protocol.Message{
		Kind: protocol.KindDecide, ApprovalID: id, Verdict: "y", Approver: "portal:me@corp",
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case d := <-askDone:
		if !d.Allow {
			t.Fatal("remote y-decision should allow")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("remote decide did not unblock Ask")
	}
}

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
