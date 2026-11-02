package desktop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wanctl/internal/adb"
	"wanctl/internal/adb/adbtest"
	"wanctl/internal/elevate"
	"wanctl/internal/opencu"
	"wanctl/internal/opencu/ocutest"
	"wanctl/internal/protocol"
)

type androidTestChannel struct {
	server *adbtest.Server
	key    *adb.Key
}

func (c *androidTestChannel) Kind() elevate.Kind { return elevate.KindADB }
func (c *androidTestChannel) Probe(context.Context) elevate.Status {
	return elevate.Status{Available: true, Link: elevate.LinkConnected}
}
func (c *androidTestChannel) Run(context.Context, string, string, io.Writer) (int, error) {
	return -1, fmt.Errorf("unexpected Run: engine must use dedicated streams")
}
func (c *androidTestChannel) Close() error { return nil }
func (c *androidTestChannel) Stream(ctx context.Context, cmd, cwd string) (elevate.Stream, error) {
	conn, err := adb.Dial(ctx, c.server.Addr, c.key, nil)
	if err != nil {
		return nil, err
	}
	return conn.Stream(ctx, cmd)
}

type androidFakeDevice struct {
	t                                *testing.T
	binary, log                      string
	mu                               sync.Mutex
	jar                              []byte
	corrupt                          bool
	startupDiagnostic                string
	starts, copies, checks, cleanups int
	alive                            atomic.Int32
	commands                         []string
}

func (d *androidFakeDevice) handle(ctx context.Context, cmd string, in io.Reader, out, stderr io.Writer) int {
	if !strings.HasPrefix(cmd, `if [ "$(id -u)" != 2000 ]`) {
		d.t.Error("shell identity guard missing", cmd)
		return 1
	}
	d.mu.Lock()
	d.commands = append(d.commands, cmd)
	d.mu.Unlock()
	switch {
	case strings.Contains(cmd, "sha256sum "):
		d.mu.Lock()
		defer d.mu.Unlock()
		d.checks++
		if len(d.jar) == 0 {
			return 1
		}
		h := sha256.Sum256(d.jar)
		fmt.Fprintf(out, "%x  /data/local/tmp/opencu-android.jar\n", h)
		return 0
	case strings.Contains(cmd, "mktemp "):
		b, err := io.ReadAll(in)
		if err != nil {
			return 1
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		d.copies++
		d.jar = b
		if d.corrupt {
			d.jar = []byte("corrupted in transit")
		}
		return 0
	case strings.Contains(cmd, "CLASSPATH="):
		d.mu.Lock()
		d.starts++
		diagnostic := d.startupDiagnostic
		d.mu.Unlock()
		if diagnostic != "" {
			io.WriteString(stderr, diagnostic)
			return 9
		}
		d.alive.Add(1)
		defer d.alive.Add(-1)
		peer := exec.CommandContext(ctx, d.binary)
		peer.Env = append(os.Environ(), "OPENCU_TEST_LOG="+d.log)
		input, err := peer.StdinPipe()
		if err != nil {
			d.t.Error(err)
			return 1
		}
		peer.Stdout, peer.Stderr = out, stderr
		if err = peer.Start(); err != nil {
			d.t.Error(err)
			return 1
		}
		copied := make(chan struct{})
		go func() { io.Copy(input, in); input.Close(); close(copied) }()
		err = peer.Wait()
		if closer, ok := in.(io.Closer); ok {
			closer.Close()
		}
		<-copied
		if err != nil {
			return 9
		}
		return 0
	case strings.Contains(cmd, "for f in /data/local/tmp/opencu-android-"):
		d.mu.Lock()
		d.cleanups++
		d.mu.Unlock()
		return 0
	default:
		d.t.Error("unexpected device command", cmd)
		return 1
	}
}
func androidHost(t *testing.T, binary string) (*AndroidOpenCU, *androidFakeDevice, *adbtest.Server) {
	t.Helper()
	d := &androidFakeDevice{t: t, binary: binary, log: filepath.Join(t.TempDir(), "calls")}
	wire := adbtest.Start(t, d.handle)
	key, err := adb.LoadOrCreateKey(t.TempDir(), "fake")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("fake Android engine jar")
	hash := sha256.Sum256(body)
	m := opencu.AndroidManifest{Name: "opencu-android", Version: "1.2.3", File: "opencu-android.jar", Size: int64(len(body)), SHA256: hex.EncodeToString(hash[:])}
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/opencu-android.json" {
			json.NewEncoder(w).Encode(m)
		} else if r.URL.Path == "/opencu-android.jar" {
			w.Write(body)
		} else {
			t.Error(r.URL.Path)
		}
	}))
	t.Cleanup(source.Close)
	host := &AndroidOpenCU{Cache: opencu.NewAndroidCache(t.TempDir(), source.URL), Elevator: elevate.NewManager(true, "", &androidTestChannel{wire, key})}
	return host, d, wire
}
func waitAndroid(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !predicate() {
		if time.Now().After(deadline) {
			t.Fatal("Android fake did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func TestAndroidOpenCURelaySessionPlacementStopAndUpdate(t *testing.T) {
	host, d, _ := androidHost(t, ocutest.Build(t))
	var version string
	host.OnVersion = func(v string) { version = v }
	s := &Service{cuIdle: 120 * time.Millisecond}
	defer s.Close()
	call := func(key string, req protocol.DesktopRequest, approve func(bool) bool) protocol.DesktopResult {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return s.DoOpenCU(ctx, key, NewID(), req, approve, host.StartOpenCU)
	}
	allow := func(bool) bool { return true }
	req := protocol.DesktopRequest{Tool: "peer_apply", Arguments: json.RawMessage(`{"exact":9007199254740993}`)}
	discovery := call("owner", protocol.DesktopRequest{Discover: true}, allow)
	if discovery.Status != "completed" || !strings.Contains(string(discovery.OpenCU.Discovery), "peer_apply") || version != "1.2.3" {
		t.Fatalf("discovery %+v version=%s", discovery, version)
	}
	first := call("owner", req, allow)
	second := call("owner", req, allow)
	if peerPID(t, first) != peerPID(t, second) || !strings.Contains(string(second.OpenCU.Result), `9007199254740993`) {
		t.Fatal("session/raw JSON changed")
	}
	active := call("owner", protocol.DesktopRequest{Tool: "peer_apply", Arguments: json.RawMessage(`{"code":"PERSON_ACTIVE","prefix":true}`)}, allow)
	if active.Warning != "Stop and ask your user; never retry automatically." || opencu.ErrorCode(active.OpenCU.Result) != "PERSON_ACTIVE" {
		t.Fatal("person-active behavior changed", active)
	}
	other := call("other-owner", req, allow)
	if peerPID(t, first) == peerPID(t, other) {
		t.Fatal("controller identities shared process")
	}
	d.mu.Lock()
	copies, checks, starts, cleanups := d.copies, d.checks, d.starts, d.cleanups
	commands := strings.Join(d.commands, "\n")
	d.mu.Unlock()
	if copies != 1 || checks != 3 || starts != 2 || cleanups != 2 {
		t.Fatalf("copies=%d checks=%d starts=%d cleanups=%d", copies, checks, starts, cleanups)
	}
	for _, want := range []string{"mktemp /data/local/tmp/.opencu-android-XXXXXXXX", `trap 'rm -f "$t"' EXIT`, `chmod 0644 "$t" && test ! -L`, `mv -f "$t"`, "exec app_process / dev.opencu.android.Main mcp"} {
		if !strings.Contains(commands, want) {
			t.Fatal("placement contract missing", want)
		}
	}
	if s.Busy() {
		t.Fatal("idle peers block update")
	}
	req.Arguments = json.RawMessage(`{"code":"USER_STOPPED"}`)
	stopped := call("owner", req, allow)
	if opencu.ErrorCode(stopped.OpenCU.Result) != "USER_STOPPED" {
		t.Fatal(stopped)
	}
	waitAndroid(t, func() bool { return d.alive.Load() == 0 })
	req.Arguments = json.RawMessage(`{}`)
	fresh := false
	denied := call("owner", protocol.DesktopRequest{Discover: true}, func(v bool) bool { fresh = v; return false })
	if !fresh || denied.OpenCU == nil || denied.OpenCU.Code != "OPENCU_STOPPED" {
		t.Fatal("idle close lost stop latch", denied)
	}
	// An unrelated controller's process failure cannot clear this owner's latch.
	crash := call("other-owner", protocol.DesktopRequest{Tool: "peer_apply", Arguments: json.RawMessage(`{"crash":true}`)}, allow)
	if crash.Status != "unknown" {
		t.Fatal(crash)
	}
	denied = call("owner", req, func(v bool) bool {
		if !v {
			t.Error("failure lost stop latch")
		}
		return false
	})
	if denied.OpenCU.Code != "OPENCU_STOPPED" {
		t.Fatal(denied)
	}
	resumed := call("owner", req, func(v bool) bool {
		if !v {
			t.Error("fresh approval missing")
		}
		return true
	})
	if resumed.Status != "completed" {
		t.Fatal(resumed)
	}
	release := s.PauseForUpdate()
	if !s.Busy() {
		t.Fatal("update lock missing")
	}
	waitAndroid(t, func() bool { return d.alive.Load() == 0 })
	release()
	if got := call("owner", req, allow); got.Status != "completed" {
		t.Fatal(got)
	}
	s.Close()
	waitAndroid(t, func() bool { return d.alive.Load() == 0 })
}
func TestAndroidOpenCUBusyDisconnectNoReplayAndRPCError(t *testing.T) {
	host, d, wire := androidHost(t, ocutest.Build(t))
	s := &Service{}
	defer s.Close()
	allow := func(bool) bool { return true }
	req := protocol.DesktopRequest{Tool: "peer_apply", Arguments: json.RawMessage(`{"rpc":true}`)}
	call := func(req protocol.DesktopRequest, gate func(bool) bool) protocol.DesktopResult {
		return s.DoOpenCU(context.Background(), "owner", NewID(), req, gate, host.StartOpenCU)
	}
	rpc := call(req, allow)
	if rpc.Status != "rejected" || !strings.Contains(string(rpc.OpenCU.RPCError), `"kept":true`) {
		t.Fatal("RPC error changed", rpc)
	}
	entered, unblock := make(chan struct{}), make(chan struct{})
	done := make(chan protocol.DesktopResult, 1)
	req.Arguments = json.RawMessage(`{}`)
	go func() { done <- call(req, func(bool) bool { close(entered); <-unblock; return true }) }()
	<-entered
	if busy := call(req, allow); !strings.Contains(busy.Error, "busy") {
		t.Fatal(busy)
	}
	close(unblock)
	if res := <-done; res.Status != "completed" {
		t.Fatal(res)
	}
	req.Arguments = json.RawMessage(`{"delay":20000}`)
	go func() { done <- call(req, allow) }()
	waitAndroid(t, func() bool { b, _ := os.ReadFile(d.log); return strings.Count(string(b), "peer_apply") == 3 })
	wire.Drop()
	select {
	case res := <-done:
		if res.Status != "unknown" || !strings.Contains(res.Error, "never replay") {
			t.Fatal(res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect hung")
	}
	b, _ := os.ReadFile(d.log)
	if strings.Count(string(b), "peer_apply") != 3 {
		t.Fatal("call replayed")
	}
	d.mu.Lock()
	starts := d.starts
	d.mu.Unlock()
	if starts != 1 {
		t.Fatal("disconnect restarted engine")
	}
}
func TestAndroidPlacementRepairsTamperingAndRefusesBadHash(t *testing.T) {
	host, d, _ := androidHost(t, ocutest.Build(t))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	first, err := host.StartOpenCU(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	d.mu.Lock()
	d.jar = []byte("tampered")
	d.mu.Unlock()
	second, err := host.StartOpenCU(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
	d.mu.Lock()
	if d.copies != 2 {
		t.Error("tampered existing jar not replaced")
	}
	d.jar = nil
	d.corrupt = true
	before := d.starts
	d.mu.Unlock()
	_, err = host.StartOpenCU(ctx)
	if err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch on device") {
		t.Fatal(err)
	}
	var startErr *openCUStartError
	if !errors.As(err, &startErr) || startErr.code != "OPENCU_NOT_INSTALLED" {
		t.Fatal("hash failure mapping", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.starts != before {
		t.Fatal("started unverified jar")
	}
}
func TestAndroidStartErrorsAreActionableAndDiagnosticsBounded(t *testing.T) {
	host, d, _ := androidHost(t, "")
	s := &Service{}
	defer s.Close()
	call := func() protocol.DesktopResult {
		return s.DoOpenCU(context.Background(), "owner", NewID(), protocol.DesktopRequest{Discover: true}, func(bool) bool { return true }, host.StartOpenCU)
	}
	host.Elevator.SetEnabled(false, "turn on 提权通道 in the wanctl app")
	res := call()
	if res.OpenCU == nil || res.OpenCU.Code != "OPENCU_START_FAILED" || !strings.Contains(res.Error, "提权通道") {
		t.Fatal(res)
	}
	if host.Cache.CachedVersion() != "" {
		t.Fatal("downloaded while disabled")
	}
	host.Elevator.SetEnabled(true, "")
	d.mu.Lock()
	d.startupDiagnostic = "HIDDEN_API_MISSING: hidden API missing\x1b" + strings.Repeat("x", 700) + "\nSECOND_LINE_PRIVATE\n"
	d.mu.Unlock()
	res = call()
	if res.OpenCU == nil || res.OpenCU.Code != "OPENCU_START_FAILED" || !strings.Contains(res.Error, `HIDDEN_API_MISSING: hidden API missing\x1b`) || strings.Contains(res.Error, "SECOND_LINE_PRIVATE") || strings.ContainsRune(res.Error, '\x1b') || len(res.Error) > 650 {
		t.Fatalf("diagnostic %q", res.Error)
	}
	d.mu.Lock()
	if d.cleanups != 0 {
		t.Error("removed old jars before successful start")
	}
	d.mu.Unlock()
	missing := httptest.NewServer(http.NotFoundHandler())
	defer missing.Close()
	host.Cache = opencu.NewAndroidCache(t.TempDir(), missing.URL)
	res = call()
	if res.OpenCU == nil || res.OpenCU.Code != "OPENCU_NOT_INSTALLED" || !strings.Contains(res.Error, "opencu-android.json download: HTTP 404") {
		t.Fatal(res)
	}
}
