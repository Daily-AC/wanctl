// Package wsconn adapts a coder/websocket connection into a net.Conn so the
// existing TLS handshake and framed protocol can run unchanged over a relayed
// WebSocket instead of a raw TCP socket.
package wsconn

import (
	"context"
	"net"
	"net/http"
	"sync"

	"wanctl/internal/protocol"

	"github.com/coder/websocket"
)

// MaxMessageBytes is the largest message either end reads: the largest frame
// the protocol carries (protocol.MaxFrame), with room for the headers of the
// TLS records it travels in. Messages are TLS records in practice, and the
// relay's pipe forwards at most 32 KiB at a time, so nothing honest comes
// near it; a peer that sends more is cut off rather than read on.
const MaxMessageBytes = protocol.MaxFrame + 64<<10

// A note on the context passed to websocket.NetConn below: it must NOT be the
// caller's dial context. Callers routinely dial under a handshake deadline
// (context.WithTimeout(ctx, 10*time.Second), cancelled the moment Dial
// returns), and binding the conn to that context would kill every connection
// immediately after it was established. The conn therefore outlives any
// context by construction; use CloseOnCancel to tie it to one deliberately.

// Dial opens a ws:// or wss:// connection and returns it as a net.Conn carrying
// binary messages. The returned *http.Response exposes handshake response
// headers/status (useful for surfacing relay auth errors).
func Dial(ctx context.Context, url string, header http.Header) (net.Conn, *http.Response, error) {
	return DialWith(ctx, url, header, nil)
}

// DialWith is Dial with an explicit *http.Client for the handshake request.
// A nil client uses the default one.
func DialWith(ctx context.Context, url string, header http.Header, hc *http.Client) (net.Conn, *http.Response, error) {
	c, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header, HTTPClient: hc})
	if err != nil {
		return nil, resp, err
	}
	return netConn(c), resp, nil
}

// FromAccepted wraps a server-side accepted websocket into a net.Conn.
func FromAccepted(ctx context.Context, c *websocket.Conn) net.Conn {
	return netConn(c)
}

// netConn wraps c for binary messages of up to MaxMessageBytes. The limit is
// set after websocket.NetConn, which lifts any limit set before it.
func netConn(c *websocket.Conn) net.Conn {
	nc := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
	c.SetReadLimit(MaxMessageBytes)
	return nc
}

// CloseOnCancel closes nc once ctx is done, which unblocks whatever read or
// write is parked on it. Without this a goroutine blocked in Read on a quiet
// connection never notices cancellation: the conn is deliberately not bound to
// any context (see the note above), so cancelling ctx alone changes nothing.
//
// That is not a theoretical concern — an agent whose control channel sits idle
// in Decode ignored SIGTERM entirely, so `wanctl stop` could not stop it and
// service managers had to wait out their kill timeout before SIGKILL.
//
// The returned stop function releases the watchdog goroutine; call it (defer is
// fine) when the caller is done with nc, otherwise the goroutine lives until
// ctx is done.
func CloseOnCancel(ctx context.Context, nc net.Conn) (stop func()) {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = nc.Close()
		case <-done:
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}
