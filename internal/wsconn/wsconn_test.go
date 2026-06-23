package wsconn

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestRoundTripBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		nc := FromAccepted(r.Context(), c)
		buf := make([]byte, 5)
		if _, err := nc.Read(buf); err != nil {
			return
		}
		nc.Write([]byte("PONG:" + string(buf)))
		time.Sleep(50 * time.Millisecond)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	nc, _, err := Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer nc.Close()
	if _, err := nc.Write([]byte("HELLO")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 64)
	n, err := nc.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); got != "PONG:HELLO" {
		t.Fatalf("got %q", got)
	}
}
