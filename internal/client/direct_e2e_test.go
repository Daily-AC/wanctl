package client

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wanctl/internal/agent"
	"wanctl/internal/direct"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
	"wanctl/internal/relay"
)

func directFixture(t *testing.T, deviceSettings, controllerSettings direct.Settings) (*agent.Agent, *Client) {
	t.Helper()
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	t.Cleanup(srv.Close)
	base := "ws" + strings.TrimPrefix(srv.URL, "http")
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	a, err := agent.New(agent.Options{RelayURL: base, Token: "tok", Name: "direct-device", AutoYes: true, Mode: policy.ModeBypass, Direct: &deviceSettings})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.Run(ctx)
	time.Sleep(150 * time.Millisecond)
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", base)
	t.Setenv("WANCTL_TOKEN", "tok")
	t.Setenv("WANCTL_TRANSPORT", "ws")
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	c.directSettings = controllerSettings
	trustServer(t, c, "direct-device")
	return a, c
}

func loopbackSettings(counter *atomic.Int32) direct.Settings {
	return direct.Settings{MinBytes: 1, STUNServers: []string{}, AllowLoopback: true, Hold: 2 * time.Second, Budget: time.Second, AckWait: time.Second, NoProgress: time.Second,
		OpenSocket: func(network string, addr *net.UDPAddr) (*net.UDPConn, error) {
			counter.Add(1)
			return net.ListenUDP(network, addr)
		}}
}

func TestDirectPushPullRoundTrip(t *testing.T) {
	var deviceSockets, controllerSockets atomic.Int32
	_, c := directFixture(t, loopbackSettings(&deviceSockets), loopbackSettings(&controllerSockets))
	payload := bytes.Repeat([]byte("direct-payload-"), 8000)
	local := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(local, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(t.TempDir(), "remote.bin")
	pushOutput := captureFileStderr(t, func() {
		if err := c.Push(context.Background(), "direct-device", local, remote); err != nil {
			t.Fatalf("direct push: %v", err)
		}
	})
	if !strings.Contains(pushOutput, "bytes, direct)\n") {
		t.Fatalf("push output = %q", pushOutput)
	}
	got, err := os.ReadFile(remote)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("push bytes: %d %v", len(got), err)
	}
	back := filepath.Join(t.TempDir(), "back.bin")
	pullOutput := captureFileStderr(t, func() {
		if err := c.Pull(context.Background(), "direct-device", remote, back); err != nil {
			t.Fatalf("direct pull: %v", err)
		}
	})
	if !strings.Contains(pullOutput, "bytes, direct)\n") {
		t.Fatalf("pull output = %q", pullOutput)
	}
	got, err = os.ReadFile(back)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("pull bytes: %d %v", len(got), err)
	}
	if deviceSockets.Load() < 2 || controllerSockets.Load() < 2 {
		t.Fatalf("no direct sockets: device=%d controller=%d", deviceSockets.Load(), controllerSockets.Load())
	}
}

func captureFileStderr(t *testing.T, run func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = old; r.Close(); w.Close() }()
	run()
	w.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDirectUDPBlockedFallsBackBothDirections(t *testing.T) {
	for _, blocked := range []string{"device", "controller"} {
		t.Run(blocked, func(t *testing.T) {
			var deviceSockets, controllerSockets atomic.Int32
			deviceSettings, controllerSettings := loopbackSettings(&deviceSockets), loopbackSettings(&controllerSockets)
			fail := func(string, *net.UDPAddr) (*net.UDPConn, error) { return nil, net.ErrClosed }
			if blocked == "device" {
				deviceSettings.OpenSocket = fail
			} else {
				controllerSettings.OpenSocket = fail
			}
			_, c := directFixture(t, deviceSettings, controllerSettings)
			payload := bytes.Repeat([]byte("fallback-first-frame"), 5000)
			local := filepath.Join(t.TempDir(), "source.bin")
			if err := os.WriteFile(local, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			remote := filepath.Join(t.TempDir(), "remote.bin")
			pushOutput := captureFileStderr(t, func() {
				if err := c.Push(context.Background(), "direct-device", local, remote); err != nil {
					t.Fatalf("fallback push: %v", err)
				}
			})
			if strings.Contains(pushOutput, "bytes, direct)") {
				t.Fatalf("false direct output: %q", pushOutput)
			}
			if got, err := os.ReadFile(remote); err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("fallback push bytes = %d, %v", len(got), err)
			}
			back := filepath.Join(t.TempDir(), "back.bin")
			pullOutput := captureFileStderr(t, func() {
				if err := c.Pull(context.Background(), "direct-device", remote, back); err != nil {
					t.Fatalf("fallback pull: %v", err)
				}
			})
			if strings.Contains(pullOutput, "bytes, direct)") {
				t.Fatalf("false direct output: %q", pullOutput)
			}
			if got, err := os.ReadFile(back); err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("fallback pull bytes = %d, %v", len(got), err)
			}
			if blocked == "device" && controllerSockets.Load() != 0 {
				t.Fatalf("controller opened %d sockets without offer", controllerSockets.Load())
			}
		})
	}
}

func TestSmallFilesOpenNoDirectSockets(t *testing.T) {
	var deviceSockets, controllerSockets atomic.Int32
	ds, cs := loopbackSettings(&deviceSockets), loopbackSettings(&controllerSockets)
	ds.MinBytes, cs.MinBytes = 8<<20, 8<<20
	_, c := directFixture(t, ds, cs)
	local := filepath.Join(t.TempDir(), "one")
	if err := os.WriteFile(local, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(t.TempDir(), "remote")
	if err := c.Push(context.Background(), "direct-device", local, remote); err != nil {
		t.Fatal(err)
	}
	if err := c.Pull(context.Background(), "direct-device", remote, filepath.Join(t.TempDir(), "back")); err != nil {
		t.Fatal(err)
	}
	if deviceSockets.Load() != 0 || controllerSockets.Load() != 0 {
		t.Fatalf("small operations opened UDP: device=%d controller=%d", deviceSockets.Load(), controllerSockets.Load())
	}
}

func TestDirectSelectionReturnsRelayReadToRequestLoop(t *testing.T) {
	var deviceSockets, controllerSockets atomic.Int32
	_, c := directFixture(t, loopbackSettings(&deviceSockets), loopbackSettings(&controllerSockets))
	relayConn, err := c.connectPipelined(context.Background(), "direct-device")
	if err != nil {
		t.Fatal(err)
	}
	defer relayConn.Close()
	remote := filepath.Join(t.TempDir(), "pending.bin")
	payload := []byte("pending-read-handoff")
	if err := protocol.WriteMessage(relayConn, protocol.Message{Kind: protocol.KindFilePut, Path: remote, Size: int64(len(payload)), Mode: 0o600, Direct: &protocol.DirectInfo{}}); err != nil {
		t.Fatal(err)
	}
	ack, err := protocol.ReadMessage(relayConn)
	if err != nil || ack.Direct == nil {
		t.Fatalf("offer = %+v %v", ack, err)
	}
	path, err := c.selectFilePath(context.Background(), relayConn, ack.Direct)
	if err != nil || !path.direct {
		t.Fatalf("selection = %+v %v", path, err)
	}
	defer path.close()
	if err := protocol.WriteFrame(path.rw, protocol.FrameData, payload); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteMessage(path.rw, protocol.Message{Kind: protocol.KindEOF}); err != nil {
		t.Fatal(err)
	}
	done, err := protocol.ReadMessage(path.rw)
	if err != nil || done.Kind != protocol.KindOK {
		t.Fatalf("direct completion = %+v %v", done, err)
	}
	if err := protocol.WriteMessage(relayConn, protocol.Message{Kind: protocol.KindStatus}); err != nil {
		t.Fatal(err)
	}
	if err := relayConn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	status, err := protocol.ReadMessage(relayConn)
	if err != nil || status.Kind != protocol.KindStatus {
		t.Fatalf("relay loop stalled: %+v %v", status, err)
	}
}

func TestLateRelayFallbackClosesRelayButNotDirectTransfer(t *testing.T) {
	var deviceSockets, controllerSockets atomic.Int32
	_, c := directFixture(t, loopbackSettings(&deviceSockets), loopbackSettings(&controllerSockets))
	relayConn, err := c.connectPipelined(context.Background(), "direct-device")
	if err != nil {
		t.Fatal(err)
	}
	defer relayConn.Close()
	remote := filepath.Join(t.TempDir(), "direct-after-relay-close.bin")
	payload := []byte("direct-survives-relay-close")
	if err := protocol.WriteMessage(relayConn, protocol.Message{Kind: protocol.KindFilePut, Path: remote, Size: int64(len(payload)), Direct: &protocol.DirectInfo{}}); err != nil {
		t.Fatal(err)
	}
	ack, err := protocol.ReadMessage(relayConn)
	if err != nil || ack.Direct == nil {
		t.Fatalf("ack = %+v %v", ack, err)
	}
	path, err := c.selectFilePath(context.Background(), relayConn, ack.Direct)
	if err != nil || !path.direct {
		t.Fatalf("direct path = %+v %v", path, err)
	}
	defer path.close()
	if err := protocol.WriteMessage(relayConn, protocol.Message{Kind: protocol.KindDirectFallback}); err != nil {
		t.Fatal(err)
	}
	answer, err := protocol.ReadMessage(relayConn)
	if err != nil || answer.Kind != protocol.KindError {
		t.Fatalf("late fallback = %+v %v", answer, err)
	}
	_ = relayConn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := protocol.ReadMessage(relayConn); err == nil {
		t.Fatal("late fallback did not close relay session")
	}
	if err := protocol.WriteFrame(path.rw, protocol.FrameData, payload); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteMessage(path.rw, protocol.Message{Kind: protocol.KindEOF}); err != nil {
		t.Fatal(err)
	}
	result, err := protocol.ReadMessage(path.rw)
	if err != nil || result.Kind != protocol.KindOK {
		t.Fatalf("direct transfer after relay close = %+v %v", result, err)
	}
	if got, err := os.ReadFile(remote); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("direct bytes = %q %v", got, err)
	}
}

func TestDirectSignalsOutsideHoldCloseSession(t *testing.T) {
	for _, kind := range []string{protocol.KindDirectOffer, protocol.KindDirectFallback} {
		t.Run(kind, func(t *testing.T) {
			var deviceSockets, controllerSockets atomic.Int32
			_, c := directFixture(t, loopbackSettings(&deviceSockets), loopbackSettings(&controllerSockets))
			conn, err := c.connectPipelined(context.Background(), "direct-device")
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := protocol.WriteMessage(conn, protocol.Message{Kind: kind}); err != nil {
				t.Fatal(err)
			}
			answer, err := protocol.ReadMessage(conn)
			if err != nil || answer.Kind != protocol.KindError {
				t.Fatalf("outside-hold reply = %+v %v", answer, err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := protocol.ReadMessage(conn); err == nil {
				t.Fatal("session remained open")
			}
		})
	}
}

func TestDirectActiveCapFifthUsesRelay(t *testing.T) {
	var deviceSockets, controllerSockets atomic.Int32
	ds, cs := loopbackSettings(&deviceSockets), loopbackSettings(&controllerSockets)
	ds.Hold = 5 * time.Second
	_, c := directFixture(t, ds, cs)
	var held []net.Conn
	defer func() {
		for _, conn := range held {
			conn.Close()
		}
	}()
	for i := 0; i < 4; i++ {
		conn, err := c.connectPipelined(context.Background(), "direct-device")
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, conn)
		path := filepath.Join(t.TempDir(), "held-"+string(rune('a'+i)))
		if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindFilePut, Path: path, Size: 1, Direct: &protocol.DirectInfo{}}); err != nil {
			t.Fatal(err)
		}
		ack, err := protocol.ReadMessage(conn)
		if err != nil || ack.Direct == nil {
			t.Fatalf("hold %d = %+v %v", i, ack, err)
		}
	}
	conn, err := c.connectPipelined(context.Background(), "direct-device")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	path := filepath.Join(t.TempDir(), "fifth")
	if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindFilePut, Path: path, Size: 1, Direct: &protocol.DirectInfo{}}); err != nil {
		t.Fatal(err)
	}
	ack, err := protocol.ReadMessage(conn)
	if err != nil || ack.Kind != protocol.KindOK || ack.Direct != nil {
		t.Fatalf("fifth offer = %+v %v", ack, err)
	}
	if err := protocol.WriteFrame(conn, protocol.FrameData, []byte("z")); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindEOF}); err != nil {
		t.Fatal(err)
	}
	if result, err := protocol.ReadMessage(conn); err != nil || result.Kind != protocol.KindOK {
		t.Fatalf("fifth result = %+v %v", result, err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "z" {
		t.Fatalf("fifth bytes = %q %v", got, err)
	}
}

func TestCompletedDirectTransfersReleaseSlotsBeforeConnectionsClose(t *testing.T) {
	var deviceSockets, controllerSockets atomic.Int32
	ds, cs := loopbackSettings(&deviceSockets), loopbackSettings(&controllerSockets)
	ds.Hold, ds.NoProgress = 5*time.Second, 5*time.Second
	_, c := directFixture(t, ds, cs)
	for i := 0; i < 4; i++ {
		conn, err := c.connectPipelined(context.Background(), "direct-device")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		remote := filepath.Join(t.TempDir(), fmt.Sprintf("completed-%d", i))
		if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindFilePut, Path: remote, Size: 1, Direct: &protocol.DirectInfo{}}); err != nil {
			t.Fatal(err)
		}
		ack, err := protocol.ReadMessage(conn)
		if err != nil || ack.Direct == nil {
			t.Fatalf("offer %d = %+v %v", i, ack, err)
		}
		path, err := c.selectFilePath(context.Background(), conn, ack.Direct)
		if err != nil || !path.direct {
			t.Fatalf("path %d = %+v %v", i, path, err)
		}
		t.Cleanup(path.close) // deliberately keep the QUIC connection open until test cleanup
		if err := protocol.WriteFrame(path.rw, protocol.FrameData, []byte{'x'}); err != nil {
			t.Fatal(err)
		}
		if err := protocol.WriteMessage(path.rw, protocol.Message{Kind: protocol.KindEOF}); err != nil {
			t.Fatal(err)
		}
		result, err := protocol.ReadMessage(path.rw)
		if err != nil || result.Kind != protocol.KindOK {
			t.Fatalf("completion %d = %+v %v", i, result, err)
		}
	}
	fifth, err := c.connectPipelined(context.Background(), "direct-device")
	if err != nil {
		t.Fatal(err)
	}
	defer fifth.Close()
	if err := protocol.WriteMessage(fifth, protocol.Message{Kind: protocol.KindFilePut, Path: filepath.Join(t.TempDir(), "fifth"), Size: 1, Direct: &protocol.DirectInfo{}}); err != nil {
		t.Fatal(err)
	}
	ack, err := protocol.ReadMessage(fifth)
	if err != nil || ack.Direct == nil {
		t.Fatalf("four completed operations still occupy slots: %+v %v", ack, err)
	}
}

func TestDirectHoldTimeoutClosesSessionAndReleasesSlot(t *testing.T) {
	var deviceSockets, controllerSockets atomic.Int32
	ds, cs := loopbackSettings(&deviceSockets), loopbackSettings(&controllerSockets)
	ds.Hold = 150 * time.Millisecond
	_, c := directFixture(t, ds, cs)
	conn, err := c.connectPipelined(context.Background(), "direct-device")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	remote := filepath.Join(t.TempDir(), "held.bin")
	if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindFilePut, Path: remote, Size: 1, Direct: &protocol.DirectInfo{}}); err != nil {
		t.Fatal(err)
	}
	ack, err := protocol.ReadMessage(conn)
	if err != nil || ack.Direct == nil {
		t.Fatalf("hold = %+v %v", ack, err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	answer, err := protocol.ReadMessage(conn)
	if err != nil || answer.Kind != protocol.KindError {
		t.Fatalf("hold timeout = %+v %v", answer, err)
	}
	if _, err := protocol.ReadMessage(conn); err == nil {
		t.Fatal("timed-out session remained open")
	}
	if _, err := os.Stat(remote); !os.IsNotExist(err) {
		t.Fatalf("timed-out upload committed: %v", err)
	}
	if temps, _ := filepath.Glob(filepath.Join(filepath.Dir(remote), ".wanctl-upload-*")); len(temps) != 0 {
		t.Fatalf("timed-out upload temps = %v", temps)
	}
	second, err := c.connectPipelined(context.Background(), "direct-device")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := protocol.WriteMessage(second, protocol.Message{Kind: protocol.KindFilePut, Path: filepath.Join(t.TempDir(), "second"), Size: 1, Direct: &protocol.DirectInfo{}}); err != nil {
		t.Fatal(err)
	}
	secondAck, err := protocol.ReadMessage(second)
	if err != nil || secondAck.Direct == nil {
		t.Fatalf("slot not available after timeout: %+v %v", secondAck, err)
	}
}

func TestDirectCompatibilityWithoutCapability(t *testing.T) {
	for _, disabled := range []string{"agent", "controller"} {
		t.Run(disabled, func(t *testing.T) {
			var deviceSockets, controllerSockets atomic.Int32
			if disabled == "agent" {
				t.Setenv("WANCTL_DIRECT", "0")
			}
			a, c := directFixture(t, loopbackSettings(&deviceSockets), loopbackSettings(&controllerSockets))
			if disabled == "agent" {
				c.directEnabled = true
			} else {
				c.directEnabled = false
			}
			if disabled == "agent" && a == nil {
				t.Fatal("agent missing")
			}
			payload := bytes.Repeat([]byte("legacy"), 12000)
			remote := filepath.Join(t.TempDir(), "remote")
			if err := c.PushBytes(context.Background(), "direct-device", remote, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			back := filepath.Join(t.TempDir(), "back")
			if err := c.Pull(context.Background(), "direct-device", remote, back); err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(back); err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("legacy bytes = %d %v", len(got), err)
			}
			if deviceSockets.Load() != 0 || controllerSockets.Load() != 0 {
				t.Fatalf("compatibility opened UDP: device=%d controller=%d", deviceSockets.Load(), controllerSockets.Load())
			}
		})
	}
}

func TestDirectPushCancellationAbortsPendingUpload(t *testing.T) {
	var deviceSockets, controllerSockets atomic.Int32
	_, c := directFixture(t, loopbackSettings(&deviceSockets), loopbackSettings(&controllerSockets))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, err := c.connectPipelined(ctx, "direct-device")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	dir := t.TempDir()
	remote := filepath.Join(dir, "cancelled.bin")
	if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindFilePut, Path: remote, Size: 1 << 20, Direct: &protocol.DirectInfo{}}); err != nil {
		t.Fatal(err)
	}
	ack, err := protocol.ReadMessage(conn)
	if err != nil || ack.Direct == nil {
		t.Fatalf("ack = %+v %v", ack, err)
	}
	path, err := c.selectFilePath(ctx, conn, ack.Direct)
	if err != nil || !path.direct {
		t.Fatalf("path = %+v %v", path, err)
	}
	defer path.close()
	if err := protocol.WriteFrame(path.rw, protocol.FrameData, []byte("partial")); err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, fileErr := os.Stat(remote)
		temps, _ := filepath.Glob(filepath.Join(dir, ".wanctl-upload-*"))
		if os.IsNotExist(fileErr) && len(temps) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancel left target or temp: file=%v temps=%v", fileErr, temps)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDirectPullCancellationNamesIncompleteFile(t *testing.T) {
	var deviceSockets, controllerSockets atomic.Int32
	_, c := directFixture(t, loopbackSettings(&deviceSockets), loopbackSettings(&controllerSockets))
	remote := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(remote, bytes.Repeat([]byte("large-file"), 100000), 0o600); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(t.TempDir(), "partial.bin")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.pullProgress = func(int64) { cancel() }
	err := c.Pull(ctx, "direct-device", remote, local)
	if err == nil || !strings.Contains(err.Error(), "incomplete") || !strings.Contains(err.Error(), local) {
		t.Fatalf("pull cancellation = %v", err)
	}
	if info, statErr := os.Stat(local); statErr != nil || info.Size() == 0 {
		t.Fatalf("partial file = %+v %v", info, statErr)
	}
}

func TestDirectPullDetectsTruncatedSourceDuringHold(t *testing.T) {
	var deviceSockets, controllerSockets atomic.Int32
	_, c := directFixture(t, loopbackSettings(&deviceSockets), loopbackSettings(&controllerSockets))
	remote := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(remote, bytes.Repeat([]byte("source"), 10000), 0o600); err != nil {
		t.Fatal(err)
	}
	conn, err := c.connectPipelined(context.Background(), "direct-device")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindFileGet, Path: remote, Direct: &protocol.DirectInfo{}}); err != nil {
		t.Fatal(err)
	}
	meta, err := protocol.ReadMessage(conn)
	if err != nil || meta.Direct == nil {
		t.Fatalf("meta = %+v %v", meta, err)
	}
	if err := os.Truncate(remote, 0); err != nil {
		t.Fatal(err)
	}
	path, err := c.selectFilePath(context.Background(), conn, meta.Direct)
	if err != nil || !path.direct {
		t.Fatalf("path = %+v %v", path, err)
	}
	defer path.close()
	local := filepath.Join(t.TempDir(), "partial.bin")
	f, err := os.Create(local)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, err = receiveFile(path.rw, f, meta.Size, local)
	if err == nil || !strings.Contains(err.Error(), "incomplete") || !strings.Contains(err.Error(), local) {
		t.Fatalf("short pull = %v", err)
	}
}

func TestWorkspaceSessionNeverOffersDirect(t *testing.T) {
	var deviceSockets, controllerSockets atomic.Int32
	_, c := directFixture(t, loopbackSettings(&deviceSockets), loopbackSettings(&controllerSockets))
	ctx := context.Background()
	ref, err := c.PrepareWorkspace(ctx, "direct-device")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if _, err := c.Workspace(ctx, ref, "open", protocol.Message{Path: root}); err != nil {
		t.Fatal(err)
	}
	defer c.Workspace(ctx, ref, "close", protocol.Message{})
	if err := os.WriteFile(filepath.Join(root, "small.txt"), []byte("workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := NewWorkspaceLink()
	c.UseWorkspaceLink(link)
	defer link.Close()
	result, err := link.exchange(ctx, c, ref, protocol.Message{Kind: protocol.KindWorkspace, Action: protocol.KindFileRead, WorkspaceID: ref.ID, Path: "small.txt", Direct: &protocol.DirectInfo{}})
	if err != nil || result.Kind != protocol.KindFileResult || result.Direct != nil {
		t.Fatalf("workspace read = %+v %v", result, err)
	}
	if deviceSockets.Load() != 0 || controllerSockets.Load() != 0 {
		t.Fatalf("workspace opened direct sockets: device=%d controller=%d", deviceSockets.Load(), controllerSockets.Load())
	}
}

// On the HTTP carrier the controller's writes are batched into /h/up requests
// and the relay forwards a request only once its body has arrived. A fallback
// sent together with the first megabyte of file data reached the device only
// after that megabyte crossed a slow uplink, past the device's hold.
func TestDirectFallbackIsNotQueuedBehindBulkUpload(t *testing.T) {
	inner := relay.New(relay.EnvTokenStore("tok:alice")).Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/h/up" && r.URL.Query().Get("role") == "client" {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if len(body) > 16<<10 {
				time.Sleep(4 * time.Second) // a slow uplink, longer than the hold
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	var deviceSockets, controllerSockets atomic.Int32
	deviceSettings, controllerSettings := loopbackSettings(&deviceSockets), loopbackSettings(&controllerSockets)
	deviceSettings.Hold = 3 * time.Second
	controllerSettings.OpenSocket = func(string, *net.UDPAddr) (*net.UDPConn, error) { return nil, net.ErrClosed }
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	a, err := agent.New(agent.Options{RelayURL: srv.URL, Token: "tok", Name: "direct-device", AutoYes: true, Transport: "http", Mode: policy.ModeBypass, Direct: &deviceSettings})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.Run(ctx)
	time.Sleep(300 * time.Millisecond)
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", srv.URL)
	t.Setenv("WANCTL_TOKEN", "tok")
	t.Setenv("WANCTL_TRANSPORT", "http")
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	c.directSettings = controllerSettings
	trustServer(t, c, "direct-device")

	payload := bytes.Repeat([]byte("slow-uplink-"), 40000)
	local := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(local, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(t.TempDir(), "remote.bin")
	if err := c.Push(context.Background(), "direct-device", local, remote); err != nil {
		t.Fatalf("fallback push over a slow uplink: %v", err)
	}
	if got, err := os.ReadFile(remote); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("pushed bytes = %d, %v", len(got), err)
	}
}
