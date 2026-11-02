package desktop

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	log          string // every peer appends its requests' methods here
	mu           sync.Mutex
	started      []*testOpenCUPeer
}

func (p *stopPeers) start(ctx context.Context) (OpenCUConnection, error) {
	cmd := exec.Command(p.binary)
	cmd.Env = append(os.Environ(), "OPENCU_TEST_STOP="+p.when)
	if p.log != "" {
		cmd.Env = append(cmd.Env, "OPENCU_TEST_LOG="+p.log)
	}
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
		{"announced while idle, connection still open", "delayed", time.Hour},
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
				// The stop comes after the act returned; wait until it is read.
				deadline := time.Now().Add(5 * time.Second)
				for !peers.first().StoppedByPerson() && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
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

func (p *stopPeers) toolCalls() int {
	b, _ := os.ReadFile(p.log)
	return strings.Count(string(b), "tools/call")
}

// stopScenario runs the first act on a fresh service and returns a function
// for later acts with the given approval callback.
func stopScenario(t *testing.T, when string) (*Service, *stopPeers, func(context.Context, func(bool) bool) protocol.DesktopResult) {
	t.Helper()
	peers := &stopPeers{binary: ocutest.BuildStopPeer(t), when: when, log: filepath.Join(t.TempDir(), "requests")}
	s := &Service{cuIdle: time.Hour}
	t.Cleanup(s.Close)
	req := protocol.DesktopRequest{Tool: "get_app_state", Arguments: json.RawMessage(`{"app":"x"}`)}
	call := func(ctx context.Context, approve func(bool) bool) protocol.DesktopResult {
		return s.DoOpenCU(ctx, "owner", NewID(), req, approve, peers.start)
	}
	return s, peers, call
}

// requireFreshApproval checks that the next act asks for fresh approval and,
// refused, opens nothing.
func requireFreshApproval(t *testing.T, peers *stopPeers, call func(context.Context, func(bool) bool) protocol.DesktopResult) {
	t.Helper()
	fresh := false
	denied := call(context.Background(), func(f bool) bool { fresh = f; return false })
	if !fresh || denied.OpenCU == nil || denied.OpenCU.Code != "OPENCU_STOPPED" || peers.count() != 1 {
		t.Fatalf("stop not latched: fresh approval=%v engines=%d result=%+v", fresh, peers.count(), denied)
	}
}

// A stop that arrives while approval for the next act is pending voids that
// approval: the act is refused as stopped and the engine receives no call.
func TestOpenCUStopDuringApprovalRefusesTheAct(t *testing.T) {
	_, peers, call := stopScenario(t, "delayed")
	if first := call(context.Background(), func(bool) bool { return true }); first.Status != "completed" {
		t.Fatalf("%+v", first)
	}
	asked := false
	res := call(context.Background(), func(fresh bool) bool {
		asked = !fresh
		deadline := time.Now().Add(5 * time.Second)
		for !peers.first().StoppedByPerson() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		return true
	})
	if !asked {
		t.Fatal("the stop was already latched before approval; the scenario did not exercise a pending approval")
	}
	if res.OpenCU == nil || res.OpenCU.Code != "OPENCU_STOPPED" || peers.toolCalls() != 1 {
		t.Fatalf("act approved before the stop ran anyway: tool calls=%d result=%+v", peers.toolCalls(), res)
	}
	requireFreshApproval(t, peers, call)
}

// A stop announced during a call that then times out (outcome unknown) is
// latched: the next act needs fresh approval rather than a new engine.
func TestOpenCUStopBeforeACallTimesOutIsLatched(t *testing.T) {
	_, peers, call := stopScenario(t, "hang")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if first := call(ctx, func(bool) bool { return true }); first.Status != "unknown" {
		t.Fatalf("%+v", first)
	}
	requireFreshApproval(t, peers, call)
}

// A stop the engine announces as it is closed for an update or an install is
// latched like one seen at an idle close.
func TestOpenCUStopSurvivesUpdateAndInstallCloses(t *testing.T) {
	for _, path := range []string{"update", "install"} {
		t.Run(path, func(t *testing.T) {
			s, peers, call := stopScenario(t, "on-exit")
			if first := call(context.Background(), func(bool) bool { return true }); first.Status != "completed" {
				t.Fatalf("%+v", first)
			}
			if path == "update" {
				s.PauseForUpdate()()
			} else {
				release, ok := s.TryPauseForInstall()
				if !ok {
					t.Fatal("desktop busy")
				}
				s.CloseIdleOpenCU()
				release()
			}
			if !peers.first().closed.Load() {
				t.Fatal("engine not closed")
			}
			requireFreshApproval(t, peers, call)
		})
	}
}

// Engine output wanctl can no longer read could hide a stop, so the next act
// needs fresh approval.
func TestOpenCUUnreadableEngineOutputNeedsFreshApproval(t *testing.T) {
	_, peers, call := stopScenario(t, "garbage")
	if first := call(context.Background(), func(bool) bool { return true }); first.Status != "completed" {
		t.Fatalf("%+v", first)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !peers.first().StoppedByPerson() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	requireFreshApproval(t, peers, call)
}

// An act on an engine that already exited is reported as not sent, not as an
// unknown outcome; the next act starts a new engine.
func TestOpenCUActOnAnExitedEngineIsNotSent(t *testing.T) {
	_, peers, call := stopScenario(t, "exit-after-call")
	allow := func(bool) bool { return true }
	if first := call(context.Background(), allow); first.Status != "completed" {
		t.Fatalf("%+v", first)
	}
	time.Sleep(300 * time.Millisecond) // the engine exits right after its reply
	res := call(context.Background(), allow)
	if res.Status != "rejected" || !strings.Contains(res.Error, "not sent") || peers.toolCalls() != 1 {
		t.Fatalf("act on an exited engine: tool calls=%d result=%+v", peers.toolCalls(), res)
	}
	if next := call(context.Background(), allow); next.Status != "completed" || peers.count() != 2 {
		t.Fatalf("next act: engines=%d result=%+v", peers.count(), next)
	}
}

// When the link to the Android engine drops, wanctl cannot know whether the
// person stopped it meanwhile, so the next act needs fresh approval.
func TestAndroidDroppedEngineLinkNeedsFreshApproval(t *testing.T) {
	host, d, wire := androidHost(t, ocutest.Build(t))
	s := &Service{cuIdle: time.Hour}
	defer s.Close()
	call := func(approve func(bool) bool) protocol.DesktopResult {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return s.DoOpenCU(ctx, "owner", NewID(), protocol.DesktopRequest{Discover: true}, approve, host.StartOpenCU)
	}
	if first := call(func(bool) bool { return true }); first.Status != "completed" {
		t.Fatalf("%+v", first)
	}
	wire.Drop()
	waitAndroid(t, func() bool {
		s.cuMu.Lock()
		defer s.cuMu.Unlock()
		e := s.cu["owner"]
		return d.alive.Load() == 0 && e != nil && e.conn != nil && personStopped(e.conn)
	})
	fresh := false
	denied := call(func(f bool) bool { fresh = f; return false })
	if !fresh || denied.OpenCU == nil || denied.OpenCU.Code != "OPENCU_STOPPED" {
		t.Fatalf("dropped link: fresh approval=%v result=%+v", fresh, denied)
	}
}
