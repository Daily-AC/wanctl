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
	c, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return nil, resp, err
	}
	c.SetReadLimit(-1) // do not cap message size; we frame in the protocol layer
	return websocket.NetConn(context.Background(), c, websocket.MessageBinary), resp, nil
}

// FromAccepted wraps a server-side accepted websocket into a net.Conn.
func FromAccepted(ctx context.Context, c *websocket.Conn) net.Conn {
	c.SetReadLimit(-1)
	return websocket.NetConn(context.Background(), c, websocket.MessageBinary)
}
