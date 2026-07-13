// Package wsconn adapts a coder/websocket connection into a net.Conn so the
// existing TLS handshake and framed protocol can run unchanged over a relayed
// WebSocket instead of a raw TCP socket.
package wsconn

import (
	"context"
	"net"
	"net/http"

	"github.com/coder/websocket"
)

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
