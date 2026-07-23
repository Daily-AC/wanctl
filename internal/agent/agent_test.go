package agent

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wanctl/internal/config"
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
