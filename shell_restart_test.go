package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"wanctl/internal/client"
	"wanctl/internal/protocol"
	"wanctl/internal/relayhttp"
	"wanctl/internal/transport"
	"wanctl/internal/wsconn"
)

// No listening socket: the HTTP server and WebSocket/TLS client communicate
// entirely over net.Pipe, with the device choosing its workspace replies.
type shellMemoryListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *shellMemoryListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *shellMemoryListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *shellMemoryListener) Addr() net.Addr { return &net.TCPAddr{Port: 80} }

func TestShellRestartEscapesDeviceDirectory(t *testing.T) {
	// relayhttp.Shared snapshots http.DefaultTransport once. Isolate that
	// initialization so this test works regardless of the rest of the suite.
	if os.Getenv("WANCTL_SHELL_RESTART_CHILD") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestShellRestartEscapesDeviceDirectory$", "-test.count=1")
		cmd.Env = append(os.Environ(), "WANCTL_SHELL_RESTART_CHILD=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("in-memory shell regression: %v\n%s", err, out)
		}
		return
	}
	id, err := transport.IdentityFromSeed(bytes.Repeat([]byte{1}, 32), "shell-controller")
	if err != nil {
		t.Fatal(err)
	}
	device, err := transport.IdentityFromSeed(bytes.Repeat([]byte{2}, 32), "shell-device")
	if err != nil {
		t.Fatal(err)
	}
	ln := &shellMemoryListener{conns: make(chan net.Conn), done: make(chan struct{})}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/resolve" {
			_ = json.NewEncoder(w).Encode(map[string]string{"target": "alice/device"})
			return
		}
		if r.URL.Path != "/dial" {
			http.NotFound(w, r)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		nc := wsconn.FromAccepted(r.Context(), ws)
		defer ws.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, _, err := transport.ServerHandshake(ctx, nc, device)
		if err != nil {
			return
		}
		if _, err = protocol.ReadMessage(conn); err != nil {
			return
		}
		if err = protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindOK}); err != nil {
			return
		}
		m, err := protocol.ReadMessage(conn)
		if err != nil {
			return
		}
		state := "open"
		if m.Action == "close" {
			state = "closed"
		}
		_ = protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindWorkspace, Workspace: &protocol.WorkspaceResult{ID: m.WorkspaceID, Root: m.Path, State: state}})
		// Read the client's TLS close_notify before closing the WebSocket.
		// Otherwise both ends wait for a close handshake and consume the
		// shell's deadline before its next /resolve request can start.
		_, _ = io.Copy(io.Discard, conn)
	})
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		a, b := net.Pipe()
		select {
		case ln.conns <- b:
			return a, nil
		case <-ctx.Done():
			a.Close()
			b.Close()
			return nil, ctx.Err()
		}
	}}
	old := http.DefaultTransport
	http.DefaultTransport = tr
	t.Cleanup(func() { tr.CloseIdleConnections(); http.DefaultTransport = old })
	// httpconn initializes relayhttp.Shared during package initialization,
	// before this test can replace http.DefaultTransport. In this isolated
	// child only, redirect its existing HTTP transport to the same memory
	// listener. Reflection is limited to accessing this test transport seam;
	// shell logic and client/protocol behavior remain the production code.
	sharedHTTP := (*http.Transport)(reflect.ValueOf(relayhttp.Shared()).Elem().FieldByName("h2").UnsafePointer())
	sharedHTTP.DialContext = tr.DialContext
	sharedHTTP.Proxy = nil
	t.Cleanup(sharedHTTP.CloseIdleConnections)
	known := transport.NewMemStore()
	if err := known.Pin("alice/device", device.Fingerprint, false); err != nil {
		t.Fatal(err)
	}
	c := client.NewWith(id, known, "ws://review.test", "review-token", "ws")
	const directory = "/tmp/a\x1b]52;c;cHJvb2Y=\a"
	s := &remoteShell{c: c, target: "alice/device", cwd: directory,
		ref: client.WorkspaceRef{Target: "alice/device", ID: "w-" + strings.Repeat("a", 32)}}
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	oldErr := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = oldErr }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.restart(ctx, "this shell reached its command limit"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.ContainsRune(got, '\x1b') || bytes.ContainsRune(got, '\a') {
		t.Fatalf("shell restart printed live device terminal controls: %q", got)
	}
}
