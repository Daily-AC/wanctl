package client

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"wanctl/internal/agent"
	"wanctl/internal/console"
	"wanctl/internal/protocol"
	"wanctl/internal/transport"
)

// TestLiveRemoteConsole runs the remote-console flow against a LIVE relay using
// this machine as the device. It is skipped unless WANCTL_LIVE_RELAY (+ DEVTOK +
// PORTALTOK) are set, so it never runs in normal CI.
//
//	WANCTL_LIVE_RELAY=https://wanctl-relay.***REMOVED***.***REMOVED***.com WANCTL_TRANSPORT=http \
//	WANCTL_LIVE_DEVTOK=<ns-***REMOVED*** token> \
//	WANCTL_LIVE_PORTALTOK=<ns-portal token> \
//	go test ./internal/client/ -run TestLiveRemoteConsole -v -timeout 120s
func TestLiveRemoteConsole(t *testing.T) {
	relay := os.Getenv("WANCTL_LIVE_RELAY")
	devTok := os.Getenv("WANCTL_LIVE_DEVTOK")
	portalTok := os.Getenv("WANCTL_LIVE_PORTALTOK")
	if relay == "" || devTok == "" || portalTok == "" {
		t.Skip("set WANCTL_LIVE_RELAY / WANCTL_LIVE_DEVTOK / WANCTL_LIVE_PORTALTOK to run the live e2e")
	}
	target := os.Getenv("WANCTL_LIVE_TARGET")
	if target == "" {
		target = "***REMOVED***/macbox"
	}
	devName := target
	if i := indexSlash(target); i >= 0 {
		devName = target[i+1:]
	}

	loadRole := func(dir string) (*transport.Identity, *transport.Store) {
		t.Setenv("WANCTL_CONFIG_DIR", dir)
		id, err := transport.LoadOrCreateIdentity()
		if err != nil {
			t.Fatal(err)
		}
		known, err := transport.OpenStore("known_servers.json")
		if err != nil {
			t.Fatal(err)
		}
		return id, known
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// This machine is the device. AutoYes auto-trusts the portal + controller.
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	ag, err := agent.New(agent.Options{
		RelayURL: relay, Token: devTok, Name: devName, AutoYes: true, Transport: "http",
	})
	if err != nil {
		t.Fatal(err)
	}
	go ag.Run(ctx)

	pid, pknown := loadRole(t.TempDir())
	portal := NewWith(pid, pknown, relay, portalTok, "http")
	cid, cknown := loadRole(t.TempDir())
	ctrl := NewWith(cid, cknown, relay, devTok, "http")

	// Wait for the device to register on the live relay.
	online := false
	for i := 0; i < 150; i++ {
		if devs, _ := ctrl.Peers(ctx); contains(devs, devName) {
			online = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !online {
		t.Fatalf("device %q never came online on %s", devName, relay)
	}
	t.Logf("device %q online on live relay", devName)

	// Portal opens a console session (cross-namespace via portalNS bypass).
	cc, err := portal.OpenConsole(ctx, target)
	if err != nil {
		t.Fatalf("OpenConsole(%q): %v", target, err)
	}
	defer cc.Close()

	notifCh := make(chan console.State, 8)
	respCh := make(chan protocol.Message, 8)
	go func() {
		for {
			m, err := protocol.ReadMessage(cc)
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
	// State round-trip proves the session is up + subscribed.
	if err := protocol.WriteMessage(cc, protocol.Message{Kind: protocol.KindConsoleState}); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-respCh:
		if m.Kind != protocol.KindConsoleState {
			t.Fatalf("state resp = %s", m.Kind)
		}
		t.Logf("portal console session established over live relay")
	case <-time.After(10 * time.Second):
		t.Fatal("no state response from device over live relay")
	}

	// CLI controller runs a rule-miss command; it blocks awaiting approval.
	type res struct {
		code int
		err  error
	}
	done := make(chan res, 1)
	go func() {
		code, err := ctrl.Exec(ctx, ExecRequest{Target: devName, Command: "echo hello-from-portal-live", OneShot: true, Cwd: ""})
		done <- res{code, err}
	}()

	var id string
	select {
	case ns := <-notifCh:
		if len(ns.Pending) != 1 {
			t.Fatalf("notif Pending = %d, want 1", len(ns.Pending))
		}
		id = ns.Pending[0].ID
		t.Logf("portal saw pending: %s cmd=%q", id, ns.Pending[0].Cmd)
	case <-time.After(15 * time.Second):
		t.Fatal("portal never received the pending approval over live relay")
	}

	// Approve remotely.
	if err := protocol.WriteMessage(cc, protocol.Message{
		Kind: protocol.KindDecide, ApprovalID: id, Verdict: "y", Approver: "portal:livecheck@corp",
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("portal approved %s", id)

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("exec error: %v", r.err)
		}
		if r.code != 0 {
			t.Fatalf("exec exit = %d, want 0", r.code)
		}
		t.Logf("LIVE e2e OK: CLI exec completed with exit 0 after remote portal approval")
	case <-time.After(15 * time.Second):
		t.Fatal("exec did not complete after remote approval over live relay")
	}
}

func indexSlash(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
