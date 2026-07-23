package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"wanctl/internal/config"
	"wanctl/internal/console"
	"wanctl/internal/eventlog"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
	"wanctl/internal/sessionauth"
	"wanctl/internal/transport"
)

func TestTrustedControllerCannotOpenAdminConsole(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	portalID, err := transport.IdentityFromSeed([]byte(strings.Repeat("p", 32)), "portal")
	if err != nil {
		t.Fatal(err)
	}
	controllerID, err := transport.IdentityFromSeed([]byte(strings.Repeat("c", 32)), "controller")
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(Options{
		RelayURL: "ws://x",
		Token:    "t",
		Name:     "dev1",
		PortalFP: portalID.Fingerprint,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.known.Add(controllerID.Fingerprint, "ordinary-controller"); err != nil {
		t.Fatal(err)
	}

	dev, controller := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go a.handleSession(ctx, dev, sessionauth.Open{
		Session:         "test-session",
		CallerNamespace: "owner",
		OwnerNamespace:  "owner",
		Device:          "dev1",
		Capabilities:    sessionauth.FullCapabilities,
	})

	dr, err := transport.ClientHandshake(ctx, controller, "dev1", controllerID, transport.NewMemStore())
	if err != nil {
		t.Fatalf("TLS handshake: %v", err)
	}
	defer controller.Close()
	if err := protocol.WriteMessage(dr.Conn, protocol.Message{
		Kind: protocol.KindConsoleHello,
		Role: "client",
		Name: "ordinary-controller",
	}); err != nil {
		t.Fatalf("console hello: %v", err)
	}
	reply, err := protocol.ReadMessage(dr.Conn)
	if err != nil {
		t.Fatalf("console hello reply: %v", err)
	}
	if reply.Kind == protocol.KindReject {
		if !strings.Contains(reply.Reason, "console administrator") {
			t.Fatalf("rejected for the wrong reason: %q", reply.Reason)
		}
		if reply.PairingURL != "" {
			t.Fatalf("console-admin rejection must not offer pairing: %q", reply.PairingURL)
		}
		if a.engine.Mode() != policy.ModeNormal {
			t.Fatalf("rejected controller changed mode to %q", a.engine.Mode())
		}
		return
	}
	if reply.Kind != protocol.KindOK {
		t.Fatalf("unexpected console hello reply: %q", reply.Kind)
	}

	if err := protocol.WriteMessage(dr.Conn, protocol.Message{
		Kind:        protocol.KindModeSet,
		ConsoleMode: string(policy.ModeBypass),
	}); err != nil {
		t.Fatalf("mode_set: %v", err)
	}
	if _, err := protocol.ReadMessage(dr.Conn); err != nil {
		t.Fatalf("mode_set reply: %v", err)
	}
	if a.engine.Mode() == policy.ModeBypass {
		t.Fatal("ordinary trusted controller opened the admin console and changed mode to bypass")
	}
	t.Fatalf("ordinary trusted controller opened the admin console; mode = %q", a.engine.Mode())
}

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

func TestPortalAdminOverlapAllowsOldKeyRevocation(t *testing.T) {
	a := newTestAgent(t)
	known, err := transport.OpenStore("known_clients.json")
	if err != nil {
		t.Fatal(err)
	}
	a.known = known
	a.portalAdmins, err = config.OpenPortalAdmins()
	if err != nil {
		t.Fatal(err)
	}
	old, newFP := testPortalFP(1), testPortalFP(2)
	if err := a.portalAdmins.Add(old, newFP); err != nil {
		t.Fatal(err)
	}
	for _, fp := range []string{old, newFP} {
		if err := known.Add(fp, "portal"); err != nil {
			t.Fatal(err)
		}
	}
	resp := a.handleConsoleRPC(protocol.Message{Kind: protocol.KindTrustRevoke, FP: old})
	if len(resp.Data) != 0 || known.Has(old) || !known.Has(newFP) {
		t.Fatalf("old portal key was not revoked after overlap: resp=%s old=%v new=%v", resp.Data, known.Has(old), known.Has(newFP))
	}
	if a.authorize(old, "portal", "") {
		t.Fatal("revoked portal key was still authorized for a new session")
	}
}

func TestPortalAdminRefusesLastKeyRevocation(t *testing.T) {
	a := newTestAgent(t)
	known, err := transport.OpenStore("known_clients.json")
	if err != nil {
		t.Fatal(err)
	}
	a.known = known
	a.portalAdmins, err = config.OpenPortalAdmins()
	if err != nil {
		t.Fatal(err)
	}
	last := testPortalFP(3)
	if err := a.portalAdmins.Add(last); err != nil {
		t.Fatal(err)
	}
	if err := known.Add(last, "portal"); err != nil {
		t.Fatal(err)
	}
	resp := a.handleConsoleRPC(protocol.Message{Kind: protocol.KindTrustRevoke, FP: last})
	if len(resp.Data) == 0 || !known.Has(last) {
		t.Fatalf("last portal key revocation was not blocked: resp=%s trusted=%v", resp.Data, known.Has(last))
	}
}

func TestRevokingOrdinaryControllerDoesNotPromoteItToPortalAdmin(t *testing.T) {
	a := newTestAgent(t)
	known, err := transport.OpenStore("known_clients.json")
	if err != nil {
		t.Fatal(err)
	}
	a.known = known
	a.portalAdmins, err = config.OpenPortalAdmins()
	if err != nil {
		t.Fatal(err)
	}
	ordinary := testPortalFP(4)
	if err := known.Add(ordinary, "controller"); err != nil {
		t.Fatal(err)
	}
	resp := a.handleConsoleRPC(protocol.Message{Kind: protocol.KindTrustRevoke, FP: ordinary})
	if len(resp.Data) != 0 || known.Has(ordinary) || a.portalAdmins.Contains(ordinary) {
		t.Fatalf("ordinary revoke changed portal admins: resp=%s trusted=%v admin=%v", resp.Data, known.Has(ordinary), a.portalAdmins.Contains(ordinary))
	}
}

func testPortalFP(value byte) string {
	return "SHA256:" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat(string([]byte{value}), 32)))
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

// TestConsoleRPCLogs proves the console session serves the device event log so
// the portal's activity timeline can show what ran, whether it was allowed, and
// the exit code.
func TestConsoleRPCLogs(t *testing.T) {
	a := newTestAgent(t)
	logger, err := eventlog.Open("events.jsonl")
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	a.log = logger
	exit := 0
	if err := logger.Append(eventlog.Event{Type: "exec", Detail: "echo hi", Decision: "approved", Exit: &exit}); err != nil {
		t.Fatalf("append: %v", err)
	}
	resp := a.handleConsoleRPC(protocol.Message{Kind: protocol.KindLogs})
	if resp.Kind != protocol.KindLogs {
		t.Fatalf("want kind %q, got %q", protocol.KindLogs, resp.Kind)
	}
	var events []eventlog.Event
	if err := json.Unmarshal(resp.Data, &events); err != nil {
		t.Fatalf("Data not []Event: %v (%s)", err, resp.Data)
	}
	if len(events) != 1 || events[0].Detail != "echo hi" || events[0].Decision != "approved" {
		t.Fatalf("unexpected events: %+v", events)
	}
}

// TestConsoleRPCLogsNilLogger proves a session without a wired logger returns an
// empty JSON array (never null, never a panic).
func TestConsoleRPCLogsNilLogger(t *testing.T) {
	a := newTestAgent(t) // a.log == nil
	resp := a.handleConsoleRPC(protocol.Message{Kind: protocol.KindLogs})
	if resp.Kind != protocol.KindLogs {
		t.Fatalf("kind = %q", resp.Kind)
	}
	var events []eventlog.Event
	if err := json.Unmarshal(resp.Data, &events); err != nil {
		t.Fatalf("Data not []Event: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("want empty, got %d", len(events))
	}
}
