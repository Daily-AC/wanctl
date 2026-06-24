package portal

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"wanctl/internal/console"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
)

// fakeDevice plays the agent side over a net.Pipe: replies to console_state and
// pushes one approval_notif.
func fakeDevice(t *testing.T, nc net.Conn) {
	go func() {
		for {
			m, err := protocol.ReadMessage(nc)
			if err != nil {
				return
			}
			switch m.Kind {
			case protocol.KindConsoleState:
				b, _ := json.Marshal(console.State{Mode: policy.ModeNormal})
				protocol.WriteMessage(nc, protocol.Message{Kind: protocol.KindConsoleState, Data: b})
			case protocol.KindModeSet:
				protocol.WriteMessage(nc, protocol.Message{Kind: protocol.KindOK})
			}
		}
	}()
}

func TestDeviceConnRPCAndNotif(t *testing.T) {
	cli, srv := net.Pipe()
	fakeDevice(t, srv)
	d := newDeviceConn(cli)
	defer d.close()

	st, err := d.state()
	if err != nil || st.Mode != policy.ModeNormal {
		t.Fatalf("state: %+v err=%v", st, err)
	}
	if err := d.setMode("bypass"); err != nil {
		t.Fatalf("setMode: %v", err)
	}

	// server pushes a notif out of band
	b, _ := json.Marshal(console.State{Mode: policy.ModeBypass})
	protocol.WriteMessage(srv, protocol.Message{Kind: protocol.KindApprovalNotif, Data: b})
	select {
	case n := <-d.notifs():
		if n.Mode != policy.ModeBypass {
			t.Fatalf("notif mode: %s", n.Mode)
		}
	case <-time.After(time.Second):
		t.Fatal("no notif")
	}
}

// TestDeviceConnRPCTimeout reproduces a half-open relayed conn: the agent
// process is gone but our transport leg stays up, so readLoop never errors. rpc
// must time out (not hang) and tear the conn down so the pool re-dials.
func TestDeviceConnRPCTimeout(t *testing.T) {
	old := rpcTimeout
	rpcTimeout = 50 * time.Millisecond
	defer func() { rpcTimeout = old }()

	cli, srv := net.Pipe()
	defer srv.Close()
	// Drain requests so our synchronous net.Pipe write unblocks, but never reply.
	go func() {
		for {
			if _, err := protocol.ReadMessage(srv); err != nil {
				return
			}
		}
	}()
	d := newDeviceConn(cli)

	start := time.Now()
	if _, err := d.state(); err == nil {
		t.Fatal("expected timeout error from unresponsive device, got nil")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("rpc blocked %s — timeout not honored", elapsed)
	}
	if d.alive() {
		t.Fatal("timed-out conn must not be alive, so the pool re-dials")
	}
}

func TestDeviceConnAlive(t *testing.T) {
	cli, srv := net.Pipe()
	defer srv.Close()
	d := newDeviceConn(cli)
	if !d.alive() {
		t.Fatal("fresh conn should be alive")
	}
	d.close()
	if d.alive() {
		t.Fatal("closed conn must not be alive (so the pool re-dials)")
	}
}
