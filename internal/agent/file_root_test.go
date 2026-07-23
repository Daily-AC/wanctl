package agent

import (
	"context"
	"crypto/tls"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wanctl/internal/policy"
	"wanctl/internal/protocol"
	"wanctl/internal/relay"
	"wanctl/internal/transport"
	"wanctl/internal/wsconn"
)

func filePolicyConn(t *testing.T, allowedRoot string) *tls.Conn {
	t.Helper()
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	t.Cleanup(srv.Close)
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	ag, err := New(Options{RelayURL: base, Token: "tok", Name: "home-pc", AutoYes: true, Mode: policy.ModeNormal})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []policy.Kind{policy.KindRead, policy.KindWrite} {
		if err := ag.engine.Add(policy.Rule{Kind: kind, Pattern: allowedRoot, Scope: policy.ScopeDir}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go ag.Run(ctx)
	time.Sleep(200 * time.Millisecond)

	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	cid, err := transport.LoadOrCreateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	known, err := transport.OpenStore("known_servers.json")
	if err != nil {
		t.Fatal(err)
	}
	dctx, dcancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(dcancel)
	nc, _, err := wsconn.Dial(dctx, base+"/dial?token=tok&target=alice/home-pc", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	dr, err := transport.ClientHandshake(dctx, nc, "home-pc", cid, known)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	t.Cleanup(func() { dr.Conn.Close() })
	if err := protocol.WriteMessage(dr.Conn, protocol.Message{Kind: protocol.KindHello, Role: "client", Name: "tester", Version: "1"}); err != nil {
		t.Fatal(err)
	}
	if reply, err := protocol.ReadMessage(dr.Conn); err != nil || reply.Kind != protocol.KindOK {
		t.Fatalf("hello: reply=%+v err=%v", reply, err)
	}
	return dr.Conn
}

func TestFilePolicyRootRejectsSymlinkEscape(t *testing.T) {
	t.Run("get", func(t *testing.T) {
		base := t.TempDir()
		allowed := filepath.Join(base, "allowed")
		outside := filepath.Join(base, "outside")
		if err := os.MkdirAll(allowed, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(outside, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside-secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(allowed, "link")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}

		conn := filePolicyConn(t, allowed)
		path := filepath.Join(allowed, "link", "secret")
		if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindFileGet, Path: path}); err != nil {
			t.Fatal(err)
		}
		reply, err := protocol.ReadMessage(conn)
		if err != nil {
			t.Fatal(err)
		}
		if reply.Kind != protocol.KindError {
			t.Fatalf("symlink escape GET reply = %q, want %q", reply.Kind, protocol.KindError)
		}
	})

	t.Run("put", func(t *testing.T) {
		base := t.TempDir()
		allowed := filepath.Join(base, "allowed")
		outside := filepath.Join(base, "outside")
		if err := os.MkdirAll(allowed, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(outside, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(allowed, "link")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}

		conn := filePolicyConn(t, allowed)
		path := filepath.Join(allowed, "link", "created")
		if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindFilePut, Path: path, Mode: 0o600}); err != nil {
			t.Fatal(err)
		}
		reply, err := protocol.ReadMessage(conn)
		if err != nil {
			t.Fatal(err)
		}
		if reply.Kind != protocol.KindError {
			t.Fatalf("symlink escape PUT reply = %q, want %q", reply.Kind, protocol.KindError)
		}
		if _, err := os.Stat(filepath.Join(outside, "created")); !os.IsNotExist(err) {
			t.Fatalf("outside file was created: %v", err)
		}
	})
}

func TestFilePolicyRootAllowsRegularFileRoundTrip(t *testing.T) {
	allowed := t.TempDir()
	conn := filePolicyConn(t, allowed)
	path := filepath.Join(allowed, "regular.txt")
	payload := []byte("inside-root")

	if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindFilePut, Path: path, Mode: 0o600}); err != nil {
		t.Fatal(err)
	}
	if reply, err := protocol.ReadMessage(conn); err != nil || reply.Kind != protocol.KindOK {
		t.Fatalf("put ack: reply=%+v err=%v", reply, err)
	}
	if err := protocol.WriteFrame(conn, protocol.FrameData, payload); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindEOF}); err != nil {
		t.Fatal(err)
	}
	if reply, err := protocol.ReadMessage(conn); err != nil || reply.Kind != protocol.KindOK {
		t.Fatalf("put result: reply=%+v err=%v", reply, err)
	}

	if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindFileGet, Path: path}); err != nil {
		t.Fatal(err)
	}
	if reply, err := protocol.ReadMessage(conn); err != nil || reply.Kind != protocol.KindFileMeta {
		t.Fatalf("get metadata: reply=%+v err=%v", reply, err)
	}
	frameType, got, err := protocol.ReadFrame(conn)
	if err != nil {
		t.Fatal(err)
	}
	if frameType != protocol.FrameData || string(got) != string(payload) {
		t.Fatalf("get data: type=%v payload=%q", frameType, got)
	}
	if reply, err := protocol.ReadMessage(conn); err != nil || reply.Kind != protocol.KindEOF {
		t.Fatalf("get eof: reply=%+v err=%v", reply, err)
	}
}
