package relay

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wanctl/internal/wsconn"
)

// startAgent connects to /agent, registers a device, and for each "open"
// control message dials the session and echoes a fixed greeting.
func startAgent(t *testing.T, base, token, device string) {
	t.Helper()
	ctx := context.Background()
	ctrl, _, err := wsconn.Dial(ctx, base+"/agent?token="+token, nil)
	if err != nil {
		t.Fatalf("agent ctrl dial: %v", err)
	}
	enc := json.NewEncoder(ctrl)
	dec := json.NewDecoder(bufio.NewReader(ctrl))
	if err := enc.Encode(map[string]string{"op": "register", "device": device}); err != nil {
		t.Fatalf("register: %v", err)
	}
	go func() {
		defer ctrl.Close()
		for {
			var msg map[string]string
			if err := dec.Decode(&msg); err != nil {
				return
			}
			if msg["op"] != "open" {
				continue
			}
			sess, _, err := wsconn.Dial(ctx, base+msg["url"], nil)
			if err != nil {
				return
			}
			go func() {
				defer sess.Close()
				buf := make([]byte, 16)
				n, _ := sess.Read(buf)
				sess.Write([]byte("echo:" + string(buf[:n])))
				time.Sleep(50 * time.Millisecond)
			}()
		}
	}()
	time.Sleep(100 * time.Millisecond) // let registration land
}

func TestRelayPipesSession(t *testing.T) {
	ts := EnvTokenStore("tok-alice:alice")
	srv := httptest.NewServer(New(ts).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	startAgent(t, base, "tok-alice", "home-pc")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := wsconn.Dial(ctx, base+"/dial?token=tok-alice&target=alice/home-pc", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.Write([]byte("ping"))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); got != "echo:ping" {
		t.Fatalf("got %q", got)
	}
}

func TestRelayRejectsBadToken(t *testing.T) {
	srv := httptest.NewServer(New(EnvTokenStore("tok-alice:alice")).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, resp, err := wsconn.Dial(ctx, base+"/dial?token=bad&target=alice/home-pc", nil)
	if err == nil {
		t.Fatal("expected rejection")
	}
	if resp != nil && resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}
