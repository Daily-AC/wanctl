package wsconn

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type readResult struct {
	n   int64
	err error
}

// Neither end reads a message of any size its peer cares to send: the limit
// is the largest frame the protocol carries, with room for the TLS records it
// travels in, and a message past it ends the connection.
func TestAcceptedConnReadsUpToTheLargestFrame(t *testing.T) {
	got := make(chan readResult, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		n, err := io.Copy(io.Discard, FromAccepted(r.Context(), c))
		got <- readResult{n, err}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	c.SetReadLimit(-1)
	if err := c.Write(ctx, websocket.MessageBinary, make([]byte, MaxMessageBytes)); err != nil {
		t.Fatalf("a message of the largest size: %v", err)
	}
	c.Write(ctx, websocket.MessageBinary, make([]byte, MaxMessageBytes+1<<20))
	select {
	case res := <-got:
		if res.err == nil {
			t.Fatal("the connection read on past a message over the limit")
		}
		// The library reads one byte past the limit before it refuses.
		if res.n < MaxMessageBytes || res.n > 2*MaxMessageBytes+1 {
			t.Fatalf("read %d bytes before stopping, want the first message whole and the second cut off", res.n)
		}
	case <-ctx.Done():
		t.Fatal("the reader never stopped")
	}
}

func TestDialedConnReadsUpToTheLargestFrame(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		c.Write(r.Context(), websocket.MessageBinary, make([]byte, MaxMessageBytes))
		c.Write(r.Context(), websocket.MessageBinary, make([]byte, MaxMessageBytes+1<<20))
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	nc, _, err := Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	got := make(chan readResult, 1)
	go func() {
		n, err := io.Copy(io.Discard, nc)
		got <- readResult{n, err}
	}()
	select {
	case res := <-got:
		if res.err == nil || res.n < MaxMessageBytes || res.n > 2*MaxMessageBytes+1 {
			t.Fatalf("read %d bytes, err %v; want the first message whole and an error on the second", res.n, res.err)
		}
	case <-ctx.Done():
		t.Fatal("the reader never stopped")
	}
}
