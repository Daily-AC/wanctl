package mcp

import (
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
	mcpapi "github.com/mark3labs/mcp-go/mcp"
	"wanctl/internal/mcpauth"
	"wanctl/internal/protocol"
	"wanctl/internal/relayhttp"
	"wanctl/internal/transport"
	"wanctl/internal/wsconn"
)

type fileMetadataListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *fileMetadataListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *fileMetadataListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *fileMetadataListener) Addr() net.Addr { return &net.TCPAddr{Port: 80} }

// File-result hashes are device metadata too. Exercise the real MCP handlers
// and client/protocol decoding with a deliberately malformed device hash.
func TestCompactFileHeadersQuoteDeviceHash(t *testing.T) {
	if os.Getenv("WANCTL_FILE_METADATA_CHILD") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCompactFileHeadersQuoteDeviceHash$", "-test.count=1")
		cmd.Env = append(os.Environ(), "WANCTL_FILE_METADATA_CHILD=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("in-memory MCP check: %v\n%s", err, output)
		}
		return
	}
	device := seededIdentity(t, "file-device")
	const hash = "abcdef\nforged header: success"
	ln := &fileMetadataListener{conns: make(chan net.Conn), done: make(chan struct{})}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		defer ws.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		conn, _, err := transport.ServerHandshake(ctx, wsconn.FromAccepted(r.Context(), ws), device)
		if err != nil {
			return
		}
		if _, err = protocol.ReadMessage(conn); err != nil {
			return
		}
		if err = protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindOK}); err != nil {
			return
		}
		req, err := protocol.ReadMessage(conn)
		if err != nil {
			return
		}
		file := &protocol.FileResult{SHA256: hash, Content: "real content\n", TotalLines: 1, FirstLine: 1, LastLine: 1, SizeBytes: 13, Replaced: 1, Created: true}
		if req.Path == "/empty" {
			file.Content = ""
			file.TotalLines = 0
			file.SizeBytes = 0
		}
		_ = protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindFileResult, File: file})
		_, _ = io.Copy(io.Discard, conn)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	// Same test-only transport seam as shell_restart_test.go; the child keeps
	// the shared client's initialization independent of other tests.
	shared := (*http.Transport)(reflect.ValueOf(relayhttp.Shared()).Elem().FieldByName("h2").UnsafePointer())
	shared.Proxy = nil
	shared.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		a, b := net.Pipe()
		select {
		case ln.conns <- b:
			return a, nil
		case <-ctx.Done():
			a.Close()
			b.Close()
			return nil, ctx.Err()
		}
	}
	http.DefaultTransport = shared // wsconn's default client uses this transport.
	t.Cleanup(shared.CloseIdleConnections)
	t.Setenv("WANCTL_RELAY", "ws://file.test")
	t.Setenv("WANCTL_TRANSPORT", "ws")
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	sessions = &sessionStore{seed: []byte(testSeed), m: map[string]*remoteSession{}}
	claim := mcpauth.Claim{Namespace: "alice", Token: "file-token", JTI: "file-metadata"}
	r := sessions.oauthSession(claim).(*remoteSession)
	if err := r.known.Pin("alice/device", device.Fingerprint, false); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), oauthClaimKey{}, claim)
	for _, tc := range []struct{ name, path string }{{"wanctl_read", "/file"}, {"wanctl_read", "/empty"}, {"wanctl_edit", "/file"}, {"wanctl_write", "/file"}} {
		t.Run(tc.name+tc.path, func(t *testing.T) {
			req := mcpapi.CallToolRequest{}
			req.Params.Arguments = map[string]any{"target": "alice/device", "path": tc.path, "old": "old", "new": "new", "content": "text"}
			h := mcpRead
			if tc.name == "wanctl_edit" {
				h = mcpEdit
			}
			if tc.name == "wanctl_write" {
				h = mcpWrite
			}
			result, err := escapeDeviceOutput(tc.name, h)(ctx, req)
			if err != nil || result.IsError {
				t.Fatalf("tool failed: %v %+v", err, result)
			}
			text := toolText(result)
			if strings.Contains(text, "\nforged header:") {
				t.Fatalf("device hash injected a header line: %q", text)
			}
		})
	}
}
