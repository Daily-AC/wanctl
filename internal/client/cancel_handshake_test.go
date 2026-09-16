package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wanctl/internal/protocol"
	"wanctl/internal/transport"
	"wanctl/internal/wsconn"

	"github.com/coder/websocket"
)

// stalledDevice serves real WebSocket sockets carrying mutual TLS. It consumes
// either the hello or file-put request and deliberately never acknowledges it.
func stalledDevice(t *testing.T, upload bool) (*Client, <-chan error) {
	t.Helper()
	serverIdentity, err := transport.IdentityFromSeed(bytes.Repeat([]byte{1}, 32), "stalled-device")
	if err != nil {
		t.Fatal(err)
	}
	clientIdentity, err := transport.IdentityFromSeed(bytes.Repeat([]byte{2}, 32), "cancel-controller")
	if err != nil {
		t.Fatal(err)
	}
	const target = "alice/stalled-device"
	ready := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/resolve" {
			_ = json.NewEncoder(w).Encode(map[string]string{"target": target})
			return
		}
		if req.URL.Path != "/dial" {
			http.NotFound(w, req)
			return
		}
		ws, err := websocket.Accept(w, req, nil)
		if err != nil {
			ready <- err
			return
		}
		defer ws.CloseNow()
		// httptest.Server cannot close hijacked sockets, so explicitly release
		// even a broken implementation when the regression test times out.
		t.Cleanup(func() { _ = ws.CloseNow() })
		conn, _, err := transport.ServerHandshake(req.Context(), wsconn.FromAccepted(req.Context(), ws), serverIdentity)
		if err != nil {
			ready <- err
			return
		}
		hello, err := protocol.ReadMessage(conn)
		if err != nil || hello.Kind != protocol.KindHello {
			ready <- fmt.Errorf("hello = %q: %v", hello.Kind, err)
			return
		}
		if upload {
			if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindOK}); err != nil {
				ready <- err
				return
			}
			put, err := protocol.ReadMessage(conn)
			if err != nil || put.Kind != protocol.KindFilePut {
				ready <- fmt.Errorf("file request = %q: %v", put.Kind, err)
				return
			}
		}
		ready <- nil
		// Block until cancellation closes the real socket; no synthetic timeout
		// or server response is allowed to make the controller return.
		_, _ = protocol.ReadMessage(conn)
	}))
	t.Cleanup(srv.Close)
	known := transport.NewMemStore()
	if err := known.Pin(target, serverIdentity.Fingerprint, false); err != nil {
		t.Fatal(err)
	}
	return NewWith(clientIdentity, known, "ws"+strings.TrimPrefix(srv.URL, "http"), "test-token", "ws"), ready
}

func TestCancelledContextClosesPostTLSHello(t *testing.T) {
	c, ready := stalledDevice(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := c.Pair(ctx, "alice/stalled-device")
		done <- err
	}()
	assertStalledOperationCancels(t, ready, done, cancel)
}

func TestCancelledContextClosesUploadAcknowledgementWait(t *testing.T) {
	c, ready := stalledDevice(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- c.PushBytes(ctx, "alice/stalled-device", "note.txt", []byte("test"), 0o600)
	}()
	assertStalledOperationCancels(t, ready, done, cancel)
}

func assertStalledOperationCancels(t *testing.T, ready, done <-chan error, cancel context.CancelFunc) {
	t.Helper()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("device did not receive the request")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled operation reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("controller remained blocked after cancellation")
	}
}
