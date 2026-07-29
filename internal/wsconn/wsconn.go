// Package wsconn adapts a coder/websocket connection into a net.Conn so the
// existing TLS handshake and framed protocol can run unchanged over a relayed
// WebSocket instead of a raw TCP socket.
package wsconn

import (
	"context"
	"net"
	"net/http"
	"sync"

	"github.com/coder/websocket"
)

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
// Pass NoProxyClient when dialing an intranet relay: corporate machines often
// export HTTP_PROXY with an empty no_proxy, which would send private-range
// dials to the proxy (and fail).
func DialWith(ctx context.Context, url string, header http.Header, hc *http.Client) (net.Conn, *http.Response, error) {
	c, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header, HTTPClient: hc})
	if err != nil {
		return nil, resp, err
	}
	c.SetReadLimit(-1) // do not cap message size; we frame in the protocol layer
	return websocket.NetConn(context.Background(), c, websocket.MessageBinary), resp, nil
}

// NoProxyClient ignores HTTP(S)_PROXY env vars. Use for intranet relay dials.
var NoProxyClient = &http.Client{Transport: &http.Transport{Proxy: nil}}

// FromAccepted wraps a server-side accepted websocket into a net.Conn.
func FromAccepted(ctx context.Context, c *websocket.Conn) net.Conn {
	c.SetReadLimit(-1)
	return websocket.NetConn(context.Background(), c, websocket.MessageBinary)
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
