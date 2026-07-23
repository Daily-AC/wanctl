package agent

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wanctl/internal/policy"
	"wanctl/internal/protocol"
	"wanctl/internal/relay"
	"wanctl/internal/transport"
	"wanctl/internal/wsconn"
)

// connectController dials the relay (WS) and completes hello; returns the conn.
func connectController(t *testing.T, base string) *transport.DialResult {
	t.Helper()
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	cid, _ := transport.LoadOrCreateIdentity()
	known, _ := transport.OpenStore("known_servers.json")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	nc, _, err := wsconn.Dial(ctx, base+"/dial?token=tok&target=alice/home-pc", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	dr, err := transport.ClientHandshake(ctx, nc, "home-pc", cid, known)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	protocol.WriteMessage(dr.Conn, protocol.Message{Kind: protocol.KindHello, Role: "client", Name: "tester"})
	reply, _ := protocol.ReadMessage(dr.Conn)
	if reply.Kind != protocol.KindOK {
		t.Fatalf("hello: %s %s", reply.Kind, reply.Reason)
	}
	return dr
}

func startAgent(t *testing.T, base string, appr policy.Approver, mode policy.Mode) *Agent {
	t.Helper()
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	ag, err := New(Options{RelayURL: base, Token: "tok", Name: "home-pc", AutoYes: true, Mode: mode})
	if err != nil {
		t.Fatal(err)
	}
	ag.setApprover(appr)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go ag.Run(ctx)
	time.Sleep(200 * time.Millisecond)
	return ag
}

func execOnce(t *testing.T, dr *transport.DialResult, cmd, cwd string) (string, int, string) {
	t.Helper()
	protocol.WriteMessage(dr.Conn, protocol.Message{Kind: protocol.KindExec, Command: cmd, OneShot: true, Cwd: cwd})
	var out strings.Builder
	for {
		ft, p, err := protocol.ReadFrame(dr.Conn)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		switch ft {
		case protocol.FrameStdout:
			out.Write(p)
		case protocol.FrameJSON:
			m, _ := protocol.DecodeMessage(p)
			switch m.Kind {
			case protocol.KindExit:
				return out.String(), m.Code, ""
			case protocol.KindReject:
				return out.String(), -1, m.Reason
			case protocol.KindError:
				return out.String(), -1, m.Reason
			}
		}
	}
}

func TestGateAllowRemembers(t *testing.T) {
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	ag := startAgent(t, base, policy.AllowApprover{}, policy.ModeNormal)
	dr := connectController(t, base)
	defer dr.Conn.Close()

	out, code, reason := execOnce(t, dr, "echo gated-ok", "")
	if code != 0 || reason != "" || !strings.Contains(out, "gated-ok") {
		t.Fatalf("expected allow: out=%q code=%d reason=%q", out, code, reason)
	}
	// AllowApprover returns Remember=false, so nothing recorded. Now verify a
	// remembering approver records a rule that subsequently pre-approves.
	ag.setApprover(rememberGlobal{})
	execOnce(t, dr, "uname -s", "")
	if !ag.engine.Allowed(policy.Request{Kind: policy.KindExec, Cmd: "uname -s -extra"}) {
		t.Fatal("expected a global rule to be remembered for 'uname -s'")
	}
}

type rememberGlobal struct{}

func (rememberGlobal) Ask(policy.Request) policy.Decision {
	return policy.Decision{Allow: true, Remember: true, Scope: policy.ScopeGlobal}
}

func TestGateDenyRejects(t *testing.T) {
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	startAgent(t, base, policy.DenyApprover{}, policy.ModeNormal)
	dr := connectController(t, base)
	defer dr.Conn.Close()

	_, code, reason := execOnce(t, dr, "rm -rf /", "")
	if code != -1 || !strings.Contains(reason, "denied by device policy") {
		t.Fatalf("expected reject, got code=%d reason=%q", code, reason)
	}
}

func TestLogsRequireIndependentPolicyGate(t *testing.T) {
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	startAgent(t, base, policy.DenyApprover{}, policy.ModeNormal)
	dr := connectController(t, base)
	defer dr.Conn.Close()
	if err := protocol.WriteMessage(dr.Conn, protocol.Message{Kind: protocol.KindLogs}); err != nil {
		t.Fatal(err)
	}
	ft, payload, err := protocol.ReadFrame(dr.Conn)
	if err != nil {
		t.Fatal(err)
	}
	if ft != protocol.FrameJSON {
		t.Fatalf("first logs frame type = %d, want JSON rejection", ft)
	}
	msg, err := protocol.DecodeMessage(payload)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Kind != protocol.KindReject {
		t.Fatalf("trusted controller read logs without capability approval: response kind = %q", msg.Kind)
	}
}

type requestKindApprover struct{ kinds chan policy.Kind }

func (a requestKindApprover) Ask(req policy.Request) policy.Decision {
	a.kinds <- req.Kind
	return policy.Decision{Allow: true}
}

func TestLogsUseLogsCapabilityPolicyKind(t *testing.T) {
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	ap := requestKindApprover{kinds: make(chan policy.Kind, 1)}
	startAgent(t, base, ap, policy.ModeNormal)
	dr := connectController(t, base)
	defer dr.Conn.Close()
	if err := protocol.WriteMessage(dr.Conn, protocol.Message{Kind: protocol.KindLogs}); err != nil {
		t.Fatal(err)
	}
	for {
		ft, payload, err := protocol.ReadFrame(dr.Conn)
		if err != nil {
			t.Fatal(err)
		}
		if ft != protocol.FrameJSON {
			continue
		}
		msg, err := protocol.DecodeMessage(payload)
		if err != nil {
			t.Fatal(err)
		}
		if msg.Kind == protocol.KindReject {
			t.Fatalf("approved logs capability was rejected: %s", msg.Reason)
		}
		if msg.Kind == protocol.KindExit {
			break
		}
	}
	select {
	case kind := <-ap.kinds:
		if kind != policy.KindLogs {
			t.Fatalf("logs gated as %q, want %q", kind, policy.KindLogs)
		}
	default:
		t.Fatal("logs request did not enter the policy gate")
	}
}

func TestBypassModeAllowsWithoutApprover(t *testing.T) {
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	startAgent(t, base, policy.DenyApprover{}, policy.ModeBypass) // deny approver, but bypass wins
	dr := connectController(t, base)
	defer dr.Conn.Close()

	out, code, reason := execOnce(t, dr, "echo bypass-ok", "")
	if code != 0 || reason != "" || !strings.Contains(out, "bypass-ok") {
		t.Fatalf("bypass should allow: out=%q code=%d reason=%q", out, code, reason)
	}
}
