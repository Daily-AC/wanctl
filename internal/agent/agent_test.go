package agent

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wanctl/internal/config"
	"wanctl/internal/eventlog"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
	"wanctl/internal/relay"
	"wanctl/internal/transport"
	"wanctl/internal/wsconn"
)

func TestNewMigratesLegacyPortalTrustMarker(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	portalFP, ordinaryFP := testPortalFP(10), testPortalFP(11)
	known, err := transport.OpenStore("known_clients.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := known.Add(portalFP, "portal"); err != nil {
		t.Fatal(err)
	}
	if err := known.Add(ordinaryFP, "controller"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{RelayURL: "ws://x", Token: "t", Name: "dev1"}); err != nil {
		t.Fatal(err)
	}
	admins, err := config.OpenPortalAdmins()
	if err != nil {
		t.Fatal(err)
	}
	if !admins.Contains(portalFP) || admins.Contains(ordinaryFP) {
		t.Fatalf("legacy migration admins = %v", admins.List())
	}
}

func TestAgentExecOverRelay(t *testing.T) {
	// Relay.
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	// Agent identity in its own config dir, auto-trust the controller.
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	ag, err := New(Options{RelayURL: base, Token: "tok", Name: "home-pc", AutoYes: true, Mode: policy.ModeBypass})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)
	time.Sleep(200 * time.Millisecond)

	// Controller side: a separate identity, dial through the relay.
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	cid, _ := transport.LoadOrCreateIdentity()
	known, _ := transport.OpenStore("known_servers.json")
	dctx, dcancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer dcancel()
	nc, _, err := wsconn.Dial(dctx, base+"/dial?token=tok&target=alice/home-pc", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	dr, err := transport.ClientHandshake(dctx, nc, "home-pc", cid, known)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	defer dr.Conn.Close()
	protocol.WriteMessage(dr.Conn, protocol.Message{Kind: protocol.KindHello, Role: "client", Name: "tester", Version: "1"})
	reply, _ := protocol.ReadMessage(dr.Conn)
	if reply.Kind != protocol.KindOK {
		t.Fatalf("hello reply: %s %s", reply.Kind, reply.Reason)
	}
	protocol.WriteMessage(dr.Conn, protocol.Message{Kind: protocol.KindExec, Command: "echo wanctl-ok", OneShot: true})
	var out strings.Builder
	var code int
	for {
		ft, p, err := protocol.ReadFrame(dr.Conn)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if ft == protocol.FrameStdout {
			out.Write(p)
		}
		if ft == protocol.FrameJSON {
			m, _ := protocol.DecodeMessage(p)
			if m.Kind == protocol.KindExit {
				code = m.Code
				break
			}
			if m.Kind == protocol.KindError {
				t.Fatalf("remote error: %s", m.Reason)
			}
		}
	}
	if code != 0 || !strings.Contains(out.String(), "wanctl-ok") {
		t.Fatalf("exec result: code=%d out=%q", code, out.String())
	}
}

func TestConsoleApproverUnblocksGate(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	a, err := New(Options{RelayURL: "ws://x", Token: "t", Name: "dev1"})
	if err != nil {
		t.Fatal(err)
	}
	if a.console == nil {
		t.Fatal("expected a console service")
	}
	// Subscribe before calling gate so that Ask enqueues (deny-by-default when
	// no front-end is listening; real portal/TTY sessions always subscribe first).
	_, cancelSub := a.console.Subscribe()
	defer cancelSub()
	go func() {
		var id string
		for id == "" {
			if p := a.console.State().Pending; len(p) > 0 {
				id = p[0].ID
			}
		}
		a.console.Decide(id, "y")
	}()
	ok, decision := a.gate(policy.Request{Kind: policy.KindExec, Cmd: "echo hi"})
	if !ok || decision != "approved" {
		t.Fatalf("gate: ok=%v decision=%q", ok, decision)
	}
}

func TestAgentStatusReportsModeAndVersion(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	a, err := New(Options{Name: "phone", RelayURL: "ws://x", Token: "tok", Mode: policy.ModeBypass, Version: "v1.2.3-test"})
	if err != nil {
		t.Fatal(err)
	}
	got := a.status()
	if got.Kind != protocol.KindStatus || got.Name != "phone" || got.ConsoleMode != "bypass" || got.Version != "v1.2.3-test" {
		t.Fatalf("status = %+v", got)
	}
}

// TestAgentRunStopsOnContextCancel pins the fix for an agent that ignored
// SIGTERM. The WebSocket control channel is deliberately not bound to any
// context, so an idle Decode parked forever and cancellation went unnoticed —
// `wanctl stop` (which sends SIGTERM) could not stop a ws-transport agent, and
// service managers waited out their kill timeout before resorting to SIGKILL.
//
// The device stays idle for the whole test: that is the failing condition. With
// traffic flowing, Decode returns on its own and the bug hides.
func TestAgentRunStopsOnContextCancel(t *testing.T) {
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	ag, err := New(Options{RelayURL: base, Token: "tok", Name: "home-pc", AutoYes: true, Mode: policy.ModeBypass})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ag.Run(ctx) }()

	time.Sleep(300 * time.Millisecond) // let the control channel register
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of cancellation — the agent would ignore SIGTERM")
	}
}

// An auto-trusted admission is permanent (known_clients.json), so it must
// leave a record the owner can query, not only a stdout line (GitLab #30).
func TestAutoTrustAdmissionIsLogged(t *testing.T) {
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	ag, err := New(Options{RelayURL: base, Token: "tok", Name: "home-pc", AutoYes: true, Mode: policy.ModeBypass})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)
	time.Sleep(200 * time.Millisecond)

	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	cid, _ := transport.LoadOrCreateIdentity()
	known, _ := transport.OpenStore("known_servers.json")
	dctx, dcancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer dcancel()
	nc, _, err := wsconn.Dial(dctx, base+"/dial?token=tok&target=alice/home-pc", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	dr, err := transport.ClientHandshake(dctx, nc, "home-pc", cid, known)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	defer dr.Conn.Close()
	protocol.WriteMessage(dr.Conn, protocol.Message{Kind: protocol.KindHello, Role: "client", Name: "probe-mac", Label: "throwaway probe", Version: "1"})
	if reply, _ := protocol.ReadMessage(dr.Conn); reply.Kind != protocol.KindOK {
		t.Fatalf("hello reply: %s %s", reply.Kind, reply.Reason)
	}

	events, err := ag.log.Read(eventlog.Filter{Type: "trust"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("trust events = %d, want 1: %+v", len(events), events)
	}
	e := events[0]
	if e.Decision != "auto-trust" || e.PeerFP != cid.Fingerprint || e.PeerName != "probe-mac" || e.Detail != "throwaway probe" {
		t.Fatalf("trust event = %+v", e)
	}

	// A second connection from the same controller is a plain connect, not a
	// second admission.
	dr.Conn.Close()
	nc2, _, err := wsconn.Dial(dctx, base+"/dial?token=tok&target=alice/home-pc", nil)
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	dr2, err := transport.ClientHandshake(dctx, nc2, "home-pc", cid, known)
	if err != nil {
		t.Fatalf("handshake 2: %v", err)
	}
	defer dr2.Conn.Close()
	protocol.WriteMessage(dr2.Conn, protocol.Message{Kind: protocol.KindHello, Role: "client", Name: "probe-mac", Version: "1"})
	if reply, _ := protocol.ReadMessage(dr2.Conn); reply.Kind != protocol.KindOK {
		t.Fatalf("hello reply 2: %s %s", reply.Kind, reply.Reason)
	}
	if events, _ = ag.log.Read(eventlog.Filter{Type: "trust"}); len(events) != 1 {
		t.Fatalf("trust events after reconnect = %d, want still 1", len(events))
	}
}
