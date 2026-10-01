package relayhttp

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blackhole is a TCP proxy whose current connections can be made to stop
// carrying bytes in both directions without being closed — what a client sees
// when the path under an open connection dies: no RST, no FIN, just silence.
type blackhole struct {
	addr     string
	accepted atomic.Int32

	mu   sync.Mutex
	dead []*atomic.Bool
}

func newBlackhole(t *testing.T, backend string) *blackhole {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	b := &blackhole{addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			b.accepted.Add(1)
			up, err := net.Dial("tcp", backend)
			if err != nil {
				c.Close()
				continue
			}
			t.Cleanup(func() { c.Close(); up.Close() })
			dead := new(atomic.Bool)
			b.mu.Lock()
			b.dead = append(b.dead, dead)
			b.mu.Unlock()
			pipe := func(dst, src net.Conn) {
				buf := make([]byte, 32<<10)
				for {
					n, err := src.Read(buf)
					if n > 0 && !dead.Load() {
						dst.Write(buf[:n])
					}
					if err != nil {
						return
					}
				}
			}
			go pipe(up, c)
			go pipe(c, up)
		}
	}()
	return b
}

// cut silences every connection open so far; later ones work.
func (b *blackhole) cut() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, d := range b.dead {
		d.Store(true)
	}
}

// A connection whose path died must not keep swallowing requests: the HTTP/2
// health check closes it and a later request dials a new one. Without
// pingAfter and pingTimeout, every request below goes to the dead connection
// and times out, and the proxy never sees a second dial.
func TestDeadHTTP2ConnectionIsReplaced(t *testing.T) {
	oldAfter, oldTimeout := pingAfter, pingTimeout
	pingAfter, pingTimeout = 300*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { pingAfter, pingTimeout = oldAfter, oldTimeout })

	r := newRelay(t, false)
	bh := newBlackhole(t, r.host)
	tr := New(r.tlsConf)
	tr.disabled = true // HTTP/2 only, as against the production relay
	// The relay's certificate names 127.0.0.1, which the proxy also listens on.
	try := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+bh.addr+"/h/poll", nil)
		resp, err := tr.RoundTrip(req)
		if err != nil {
			return err
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil
	}
	if err := try(); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if resp := r.lastProto(); resp != "HTTP/2.0" {
		t.Fatalf("first request went over %q, want HTTP/2.0", resp)
	}
	bh.cut()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if try() == nil {
			if n := bh.accepted.Load(); n < 2 {
				t.Fatalf("a request succeeded on the cut connection (%d dials)", n)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("still failing 5 s after the path died; the proxy saw %d connection(s), so the dead one kept being reused", bh.accepted.Load())
}
