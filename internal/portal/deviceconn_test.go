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
	notifs, unsubscribe := d.subscribe()
	defer unsubscribe()

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
	case n := <-notifs:
		if n.Mode != policy.ModeBypass {
			t.Fatalf("notif mode: %s", n.Mode)
		}
	case <-time.After(time.Second):
		t.Fatal("no notif")
	}
}

func TestDeviceConnNotifFanOut(t *testing.T) {
	cli, srv := net.Pipe()
	d := newDeviceConn(cli)
	defer d.close()
	defer srv.Close()

	first, unsubscribeFirst := d.subscribe()
	defer unsubscribeFirst()
	second, unsubscribeSecond := d.subscribe()
	defer unsubscribeSecond()

	writeNotif(t, srv, policy.ModeBypass)
	assertNotifMode(t, first, policy.ModeBypass)
	assertNotifMode(t, second, policy.ModeBypass)
}

func TestDeviceConnNotifUnsubscribeIsolated(t *testing.T) {
	cli, srv := net.Pipe()
	d := newDeviceConn(cli)
	defer d.close()
	defer srv.Close()

	unsubscribed, unsubscribe := d.subscribe()
	remaining, unsubscribeRemaining := d.subscribe()
	defer unsubscribeRemaining()

	unsubscribe()
	unsubscribe()
	if _, ok := <-unsubscribed; ok {
		t.Fatal("unsubscribed notification channel is still open")
	}

	writeNotif(t, srv, policy.ModeBypass)
	assertNotifMode(t, remaining, policy.ModeBypass)
}

func TestDeviceConnSlowNotifSubscriberDoesNotBlockOthers(t *testing.T) {
	cli, srv := net.Pipe()
	d := newDeviceConn(cli)
	defer d.close()
	defer srv.Close()

	_, unsubscribeSlow := d.subscribe()
	defer unsubscribeSlow()
	fast, unsubscribeFast := d.subscribe()
	defer unsubscribeFast()

	for i := 0; i < 9; i++ {
		mode := policy.ModeNormal
		if i == 8 {
			mode = policy.ModeBypass
		}
		writeNotif(t, srv, mode)
		assertNotifMode(t, fast, mode)
	}
}

func TestDeviceConnSubscribeAfterClose(t *testing.T) {
	cli, srv := net.Pipe()
	d := newDeviceConn(cli)
	d.close()
	defer srv.Close()

	notifs, unsubscribe := d.subscribe()
	unsubscribe()
	if _, ok := <-notifs; ok {
		t.Fatal("subscription created after close is still open")
	}
}

// TestDeviceConnCloseWakesLiveSubscribers covers the resident-consumer case: a
// background watcher subscribes and then the device drops. It must observe the
// channel closing rather than parking forever on a conn that will never deliver
// again — a request-scoped consumer survived that only because its own context
// or poll deadline fired. The deferred cancel also asserts that unsubscribing a
// channel close() already reclaimed does not double-close.
func TestDeviceConnCloseWakesLiveSubscribers(t *testing.T) {
	cli, srv := net.Pipe()
	d := newDeviceConn(cli)
	defer srv.Close()

	notifs, unsubscribe := d.subscribe()
	defer unsubscribe()

	d.close()

	select {
	case _, ok := <-notifs:
		if ok {
			t.Fatal("subscription delivered a value instead of closing")
		}
	case <-time.After(time.Second):
		t.Fatal("close() left a live subscriber parked on its channel")
	}
}

func writeNotif(t *testing.T, conn net.Conn, mode policy.Mode) {
	t.Helper()
	b, err := json.Marshal(console.State{Mode: mode})
	if err != nil {
		t.Fatalf("marshal notification: %v", err)
	}
	if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindApprovalNotif, Data: b}); err != nil {
		t.Fatalf("write notification: %v", err)
	}
}

func assertNotifMode(t *testing.T, notifs <-chan console.State, want policy.Mode) {
	t.Helper()
	select {
	case st, ok := <-notifs:
		if !ok {
			t.Fatal("notification channel closed")
		}
		if st.Mode != want {
			t.Fatalf("notification mode: got %q, want %q", st.Mode, want)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for notification")
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
