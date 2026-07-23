package client

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wanctl/internal/agent"
	"wanctl/internal/console"
	"wanctl/internal/protocol"
	"wanctl/internal/relay"
	"wanctl/internal/transport"
)

// TestRemoteConsoleApprovalOverRelay is a LOCAL end-to-end run of the headline
// feature: a portal-style controller opens a console session to a device over a
// real relay tunnel, sees a CLI controller's rule-miss exec appear as a pending
// approval in real time (via the SSE-equivalent KindApprovalNotif push), and
// approves it remotely — after which the CLI exec completes with exit 0.
//
// Three independent identities (device / portal / controller), one real relay,
// real E2E mutual-TLS through the byte pipe. No Postgres, no web layer — this
// drives the protocol/relay/agent/console stack that the portal HTTP handlers
// sit on top of.
func TestRemoteConsoleApprovalOverRelay(t *testing.T) {
	// Relay: env token store maps tokens to namespaces; "portal" is privileged.
	r := relay.New(relay.EnvTokenStore("dtok:alice,ptok:portal"))
	r.SetPortalNS("portal")
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Helper: load an identity + known-servers store from a dedicated config dir
	// (each role needs its own key; config dir is process-global via env, so we
	// set it immediately before each load).
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

	// Portal-style controller (privileged token, its own identity). Its
	// fingerprint is enrolled on the device as the console-admin capability.
	pid, pknown := loadRole(t.TempDir())
	portal := NewWith(pid, pknown, base, "ptok", "ws")

	// Device "lab" in namespace alice. AutoYes auto-trusts new ordinary
	// controllers, independently of the portal-only console capability.
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	ag, err := agent.New(agent.Options{
		RelayURL: base,
		Token:    "dtok",
		Name:     "lab",
		AutoYes:  true,
		PortalFP: pid.Fingerprint,
	}) // normal mode
	if err != nil {
		t.Fatal(err)
	}
	go ag.Run(ctx)

	// CLI controller (device-namespace token, its own identity).
	cid, cknown := loadRole(t.TempDir())
	ctrl := NewWith(cid, cknown, base, "dtok", "ws")

	// Wait for the device to come online (relay registry visible to the controller).
	online := false
	for i := 0; i < 100; i++ {
		if devs, _ := ctrl.Peers(ctx); len(devs) == 1 && devs[0] == "lab" {
			online = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !online {
		t.Fatal("device never came online")
	}

	// Open the console session (portal -> device, cross-namespace via portalNS bypass).
	cc, err := portal.OpenConsole(ctx, "alice/lab")
	if err != nil {
		t.Fatalf("OpenConsole: %v", err)
	}
	defer cc.Close()

	// Reader goroutine: split async notifs from RPC responses (mirrors deviceConn).
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
	rpc := func(req protocol.Message) protocol.Message {
		if err := protocol.WriteMessage(cc, req); err != nil {
			t.Fatalf("console write: %v", err)
		}
		select {
		case m := <-respCh:
			return m
		case <-time.After(3 * time.Second):
			t.Fatal("console RPC timed out")
			return protocol.Message{}
		}
	}

	// A successful state round-trip proves the console session is established AND
	// subscribed (the agent subscribes before entering its serve loop), so a
	// subsequent exec will be queued rather than fast-denied.
	st := rpc(protocol.Message{Kind: protocol.KindConsoleState})
	if st.Kind != protocol.KindConsoleState {
		t.Fatalf("state resp kind = %s", st.Kind)
	}

	// CLI controller runs a rule-miss command; it blocks awaiting approval.
	type execResult struct {
		code int
		err  error
	}
	execDone := make(chan execResult, 1)
	go func() {
		code, err := ctrl.Exec(ctx, "lab", "echo approved-via-portal", true, "")
		execDone <- execResult{code, err}
	}()

	// The pending request is pushed to the portal in real time, as a full State.
	var pendingID string
	select {
	case ns := <-notifCh:
		if len(ns.Pending) != 1 {
			t.Fatalf("notif Pending = %d, want 1", len(ns.Pending))
		}
		if ns.Pending[0].Cmd != "echo approved-via-portal" {
			t.Fatalf("pending cmd = %q", ns.Pending[0].Cmd)
		}
		pendingID = ns.Pending[0].ID
		t.Logf("portal saw pending approval: %s  cmd=%q", pendingID, ns.Pending[0].Cmd)
	case <-time.After(3 * time.Second):
		t.Fatal("portal never received the pending approval notification")
	}

	// Approve it remotely from the portal.
	dec := rpc(protocol.Message{
		Kind: protocol.KindDecide, ApprovalID: pendingID, Verdict: "y", Approver: "portal:tester@corp",
	})
	if dec.Kind == protocol.KindError {
		t.Fatalf("decide error: %s", dec.Data)
	}
	t.Logf("portal approved %s", pendingID)

	// The CLI exec now completes successfully.
	select {
	case res := <-execDone:
		if res.err != nil {
			t.Fatalf("exec returned error: %v", res.err)
		}
		if res.code != 0 {
			t.Fatalf("exec exit code = %d, want 0", res.code)
		}
		t.Logf("CLI exec completed with exit 0 after remote approval")
	case <-time.After(3 * time.Second):
		t.Fatal("exec did not complete after remote approval")
	}
}
