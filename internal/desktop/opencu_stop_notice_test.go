package desktop

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"wanctl/internal/opencu"
	"wanctl/internal/opencu/ocutest"
	"wanctl/internal/protocol"
)

// stopPeers starts stop-announcing peers (see ocutest.BuildStopPeer) and keeps
// every one it started.
type stopPeers struct {
	binary, when string
	mu           sync.Mutex
	started      []*testOpenCUPeer
}

func (p *stopPeers) start(ctx context.Context) (OpenCUConnection, error) {
	cmd := exec.Command(p.binary)
	cmd.Env = append(os.Environ(), "OPENCU_TEST_STOP="+p.when)
	c, err := opencu.Start(ctx, cmd)
	if err != nil {
		return nil, err
	}
	peer := &testOpenCUPeer{Client: c}
	p.mu.Lock()
	p.started = append(p.started, peer)
	p.mu.Unlock()
	return peer, nil
}
func (p *stopPeers) count() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.started) }
func (p *stopPeers) first() *testOpenCUPeer {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.started[0]
}

// The person's stop, announced by the engine while no call is running, is
// latched: after the idle close, or with the connection still open, the next
// act needs fresh explicit approval, is refused without it, and then runs on a
// new engine.
func TestOpenCUStopAnnouncedOutsideACallNeedsFreshApproval(t *testing.T) {
	binary := ocutest.BuildStopPeer(t)
	for _, tc := range []struct {
		name, when string
		idle       time.Duration
	}{
		{"announced while idle, then idle close", "after-call", 80 * time.Millisecond},
		{"announced on exit at idle close", "on-exit", 80 * time.Millisecond},
		{"announced while idle, connection still open", "after-call", time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peers := &stopPeers{binary: binary, when: tc.when}
			s := &Service{cuIdle: tc.idle}
			defer s.Close()
			req := protocol.DesktopRequest{Tool: "get_app_state", Arguments: json.RawMessage(`{"app":"x"}`)}
			call := func(approve func(bool) bool) protocol.DesktopResult {
				return s.DoOpenCU(context.Background(), "owner", NewID(), req, approve, peers.start)
			}
			if first := call(func(bool) bool { return true }); first.Status != "completed" {
				t.Fatalf("%+v", first)
			}
			if tc.idle < time.Second {
				deadline := time.Now().Add(3 * time.Second)
				for !peers.first().closed.Load() && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
				if !peers.first().closed.Load() {
					t.Fatal("idle peer not closed")
				}
			} else {
				time.Sleep(300 * time.Millisecond) // the notice follows the reply at once
			}
			fresh := false
			denied := call(func(f bool) bool { fresh = f; return false })
			if !fresh || denied.OpenCU == nil || denied.OpenCU.Code != "OPENCU_STOPPED" {
				t.Fatalf("stop announced outside a call was not latched: fresh approval=%v result=%+v", fresh, denied)
			}
			if peers.count() != 1 {
				t.Fatal("an engine was started without fresh approval")
			}
			resumed := call(func(f bool) bool { fresh = f; return true })
			if !fresh || resumed.Status != "completed" || peers.count() != 2 || !peers.first().closed.Load() {
				t.Fatalf("resume after fresh approval: fresh=%v status=%s engines=%d", fresh, resumed.Status, peers.count())
			}
		})
	}
}

// On Android the engine connection reports the same stop.
func TestAndroidStopAnnouncedWhileIdleNeedsFreshApproval(t *testing.T) {
	t.Setenv("OPENCU_TEST_STOP", "after-call")
	host, d, _ := androidHost(t, ocutest.BuildStopPeer(t))
	s := &Service{cuIdle: 80 * time.Millisecond}
	defer s.Close()
	req := protocol.DesktopRequest{Tool: "get_app_state", Arguments: json.RawMessage(`{}`)}
	call := func(approve func(bool) bool) protocol.DesktopResult {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return s.DoOpenCU(ctx, "owner", NewID(), req, approve, host.StartOpenCU)
	}
	if first := call(func(bool) bool { return true }); first.Status != "completed" {
		t.Fatalf("%+v", first)
	}
	waitAndroid(t, func() bool { return d.alive.Load() == 0 })
	fresh := false
	denied := call(func(f bool) bool { fresh = f; return false })
	if !fresh || denied.OpenCU == nil || denied.OpenCU.Code != "OPENCU_STOPPED" {
		t.Fatalf("Android stop announced while idle was not latched: fresh approval=%v result=%+v", fresh, denied)
	}
}
