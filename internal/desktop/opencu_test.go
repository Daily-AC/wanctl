package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wanctl/internal/opencu"
	"wanctl/internal/opencu/ocutest"
	"wanctl/internal/protocol"
)

type testOpenCUPeer struct {
	*opencu.Client
	closed atomic.Bool
}

func (p *testOpenCUPeer) Close() { p.Client.Close(); p.closed.Store(true) }
func (p *testOpenCUPeer) Do(ctx context.Context, r protocol.DesktopRequest) (protocol.OpenCUResult, error) {
	var out protocol.OpenCUResult
	var err error
	if r.Discover {
		out.Discovery, err = p.Discover(ctx)
	} else {
		out.Result, err = p.Call(ctx, r.Tool, r.Arguments)
	}
	return out, err
}
func peerPID(t *testing.T, r protocol.DesktopResult) int {
	t.Helper()
	if r.OpenCU == nil || r.Status != "completed" {
		t.Fatalf("%+v", r)
	}
	var meta struct {
		Meta struct{ PID int } `json:"_meta"`
	}
	if json.Unmarshal(r.OpenCU.Result, &meta) != nil {
		t.Fatal("missing peer result")
	}
	return meta.Meta.PID
}
func TestOpenCUSessionLifecycleAndSerialization(t *testing.T) {
	binary := ocutest.Build(t)
	var starts atomic.Int32
	var peers []*testOpenCUPeer
	start := func(ctx context.Context) (OpenCUConnection, error) {
		c, err := opencu.Start(ctx, exec.Command(binary))
		if err != nil {
			return nil, err
		}
		p := &testOpenCUPeer{Client: c}
		peers = append(peers, p)
		starts.Add(1)
		return p, nil
	}
	s := &Service{cuIdle: 80 * time.Millisecond}
	defer s.Close()
	life, end := context.WithCancel(context.Background())
	defer end()
	allow := func(bool) bool { return true }
	req := protocol.DesktopRequest{WorkspaceID: "workspace", Tool: "peer_apply", Arguments: json.RawMessage(`{"app":"Disposable"}`)}
	call := func(r protocol.DesktopRequest, gate func(bool) bool) protocol.DesktopResult {
		return s.DoOpenCU(context.Background(), life, "owner:workspace", NewID(), r, gate, start)
	}
	a := call(req, allow)
	b := call(req, allow)
	if peerPID(t, a) != peerPID(t, b) || starts.Load() != 1 {
		t.Fatal("peer was not reused")
	}
	if s.Busy() {
		t.Fatal("idle connection blocks auto-update")
	}
	deadline := time.Now().Add(3 * time.Second)
	for !peers[0].closed.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !peers[0].closed.Load() {
		t.Fatal("idle peer not closed")
	}
	c := call(req, allow)
	if peerPID(t, c) == peerPID(t, a) || starts.Load() != 2 {
		t.Fatal("idle peer reused")
	}
	// Physical stop must survive timer expiry and update cleanup.
	req.Arguments = json.RawMessage(`{"code":"USER_STOPPED","prefix":true}`)
	if code := opencu.ErrorCode(call(req, allow).OpenCU.Result); code != "USER_STOPPED" {
		t.Fatal(code)
	}
	deadline = time.Now().Add(3 * time.Second)
	for !peers[1].closed.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !peers[1].closed.Load() {
		t.Fatal("stopped idle peer not closed")
	}
	release := s.PauseForUpdate()
	release()
	req.Arguments = json.RawMessage(`{}`)
	prompted := false
	refused := call(req, func(fresh bool) bool { prompted = fresh; return false })
	if !prompted || refused.OpenCU.Code != "OPENCU_STOPPED" || starts.Load() != 2 {
		t.Fatal("stop bypassed or reconnected")
	}
	resumed := call(req, func(fresh bool) bool {
		if !fresh {
			t.Error("fresh approval missing")
		}
		return true
	})
	if peerPID(t, resumed) == peerPID(t, c) || starts.Load() != 3 {
		t.Fatal("approval did not start a new connection")
	}
	// The lock spans approval, closing the approval/stop race as well as input.
	entered, unblock := make(chan struct{}), make(chan struct{})
	done := make(chan protocol.DesktopResult, 1)
	go func() { done <- call(req, func(bool) bool { close(entered); <-unblock; return true }) }()
	<-entered
	other := call(req, allow)
	if !strings.Contains(other.Error, "busy") {
		t.Fatal("concurrent OpenCU call admitted")
	}
	snap, _ := s.Store.Put("owner", protocol.DesktopSnapshot{}, time.Now())
	legacy, _ := s.Do(context.Background(), "owner", "label", "act", NewID(), &protocol.DesktopRequest{ScreenshotID: snap.ID, Actions: []protocol.DesktopAction{{Type: "key", Key: "enter"}}}, func(context.Context, Job) (protocol.DesktopResult, []byte, error) {
		t.Error("concurrent legacy ran")
		return protocol.DesktopResult{}, nil, nil
	})
	if !strings.Contains(legacy.Error, "busy") {
		t.Fatal("legacy lock not shared")
	}
	close(unblock)
	<-done
	end()
	s.CloseOpenCUSession("owner:workspace")
	if !peers[len(peers)-1].closed.Load() {
		t.Fatal("workspace end left child alive")
	}
	live := s.DoOpenCU(context.Background(), context.Background(), "another-workspace", NewID(), req, allow, start)
	if live.Status != "completed" {
		t.Fatalf("%+v", live)
	}
	s.Close()
	if !peers[len(peers)-1].closed.Load() {
		t.Fatal("shutdown left peer alive")
	}
	before := starts.Load()
	closed := s.DoOpenCU(context.Background(), context.Background(), "another-workspace", NewID(), req, allow, start)
	if closed.Status != "rejected" || starts.Load() != before {
		t.Fatal("closed service restarted peer")
	}
}
func TestOpenCUCrashNeverReplaysAndCancels(t *testing.T) {
	binary := ocutest.Build(t)
	log := filepath.Join(t.TempDir(), "calls")
	s := &Service{}
	defer s.Close()
	var starts int
	start := func(ctx context.Context) (OpenCUConnection, error) {
		starts++
		cmd := exec.Command(binary)
		cmd.Env = append(os.Environ(), "OPENCU_TEST_LOG="+log)
		c, e := opencu.Start(ctx, cmd)
		return &testOpenCUPeer{Client: c}, e
	}
	req := protocol.DesktopRequest{WorkspaceID: "w", Tool: "peer_apply", Arguments: json.RawMessage(`{"crash":true}`)}
	res := s.DoOpenCU(context.Background(), context.Background(), "w", NewID(), req, func(bool) bool { return true }, start)
	if res.Status != "unknown" || !strings.Contains(res.Error, "observe again") || starts != 1 {
		t.Fatalf("%+v starts=%d", res, starts)
	}
	lines, _ := os.ReadFile(log)
	if strings.Count(string(lines), "peer_apply") != 1 {
		t.Fatal("call replayed")
	}
	req.Arguments = json.RawMessage(`{"delay":20000}`)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	res = s.DoOpenCU(ctx, context.Background(), "w", NewID(), req, func(bool) bool { return true }, start)
	if res.Status != "unknown" {
		t.Fatalf("cancel: %+v", res)
	}
}
func TestOpenCUHelperManifestAndFraming(t *testing.T) {
	binary := ocutest.Build(t)
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir)
	req := protocol.DesktopRequest{Discover: true, WorkspaceID: "w"}
	// Run the real persistent helper framing with only the native session check
	// replaced. The manifest and child process are real, as on the user side.
	run := func(first protocol.DesktopRequest) (io.WriteCloser, io.ReadCloser, <-chan int) {
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		done := make(chan int, 1)
		go func() {
			done <- openCUHelperMainUsing(inR, outW, Job{Action: "act", Request: first}, func() error { return nil })
			outW.Close()
			inR.Close()
		}()
		return inW, outR, done
	}
	in, out, done := run(req)
	missing, _, err := ReadResult(out)
	if err != nil || missing.OpenCU.Code != "OPENCU_NOT_INSTALLED" || !strings.Contains(missing.OpenCU.Message, opencu.InstallCommand) {
		t.Fatalf("%+v %v", missing, err)
	}
	in.Close()
	out.Close()
	<-done
	os.Mkdir(filepath.Join(dir, "OpenCU"), 0700)
	raw, _ := json.Marshal(map[string]any{"mcp": map[string]any{"command": binary, "args": []string{}}})
	os.WriteFile(filepath.Join(dir, "OpenCU", "opencu.json"), raw, 0600)
	in, out, done = run(req)
	discovery, _, err := ReadResult(out)
	if err != nil || !bytes.Contains(discovery.OpenCU.Discovery, []byte("peer_apply")) {
		t.Fatal(err)
	}
	var pid int
	for i := 0; i < 2; i++ {
		req.Discover = false
		req.Tool = "peer_apply"
		req.Arguments = json.RawMessage(`{"large":true,"error":true}`)
		raw, _ = json.Marshal(Job{Action: "act", Request: req})
		if err = protocol.WriteFrame(in, protocol.FrameJSON, raw); err != nil {
			t.Fatal(err)
		}
		r, _, err := ReadResult(out)
		if err != nil {
			t.Fatal(err)
		}
		var peer struct {
			Meta struct{ PID int } `json:"_meta"`
		}
		json.Unmarshal(r.OpenCU.Result, &peer)
		if i == 0 {
			pid = peer.Meta.PID
		} else if pid != peer.Meta.PID {
			t.Fatal("helper reconnected")
		}
		// This result exceeds MaxFrame, exercising the actual chunk writer/reader.
		if len(r.OpenCU.Result) <= protocol.MaxFrame {
			t.Fatalf("fixture too small: %d", len(r.OpenCU.Result))
		}
	}
	in.Close()
	out.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatal(code)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("helper ignored owner EOF")
	}
}
func TestOpenCUInvalidModesAndPayload(t *testing.T) {
	for _, r := range []protocol.DesktopRequest{
		{Discover: true}, {Discover: true, Tool: "x", WorkspaceID: "w"}, {Discover: true, WorkspaceID: "w", ScreenshotID: "x"}, {Tool: "x", WorkspaceID: "w", Actions: []protocol.DesktopAction{}}, {Tool: "x", WorkspaceID: "w", Arguments: json.RawMessage(`null`)},
	} {
		if Validate("act", &r) == nil {
			t.Fatalf("accepted %+v", r)
		}
	}
	var wire bytes.Buffer
	protocol.WriteMessage(&wire, protocol.Message{Kind: protocol.KindDesktopResult, DesktopResult: &protocol.DesktopResult{OpenCUBytes: opencu.MaxResult + 1}})
	if _, _, err := ReadResult(&wire); err == nil {
		t.Fatal("unbounded allocation")
	}
}
