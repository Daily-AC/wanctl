// Package httpconn provides a net.Conn that tunnels bytes over plain HTTP, so
// the end-to-end TLS handshake and framed protocol can run through any reverse
// proxy — including ones (like thunderbox's nginx edge) that do not forward the
// WebSocket Upgrade header. It is the proxy-agnostic alternative to wsconn.
//
// A session uses two HTTP request shapes against the relay:
//
//	down: GET  /h/down?session=&role=&token=   — a long-lived chunked response
//	      carrying relay->endpoint bytes (relay sets X-Accel-Buffering: no so
//	      nginx streams it instead of buffering).
//	up:   POST /h/up?session=&role=&token=     — one request per Write, body =
//	      the bytes; nginx buffers the small body then forwards it, so per-chunk
//	      POSTs work where a single streaming POST body would not.
package httpconn

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

type conn struct {
	base    string // http(s)://host
	session string
	role    string
	token   string
	hc      *http.Client

	down   io.ReadCloser
	writeM sync.Mutex
	closed bool
}

// Dial opens the down stream for a session/role and returns a net.Conn. base is
// the relay's HTTP origin (http:// or https://, no path).
func Dial(ctx context.Context, base, session, role, token string) (net.Conn, error) {
	base = normalizeBase(base)
	hc := &http.Client{Timeout: 0} // no overall timeout: streams are long-lived
	q := url.Values{"session": {session}, "role": {role}, "token": {token}}
	req, err := http.NewRequestWithContext(context.Background(), "GET", base+"/h/down?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("down stream: relay returned %d", resp.StatusCode)
	}
	return &conn{base: base, session: session, role: role, token: token, hc: hc, down: resp.Body}, nil
}

func normalizeBase(b string) string {
	if len(b) >= 4 && b[:4] == "wss:" {
		return "https:" + b[4:]
	}
	if len(b) >= 3 && b[:3] == "ws:" {
		return "http:" + b[3:]
	}
	return b
}

func (c *conn) Read(p []byte) (int, error) { return c.down.Read(p) }

func (c *conn) Write(p []byte) (int, error) {
	c.writeM.Lock()
	defer c.writeM.Unlock()
	if c.closed {
		return 0, io.ErrClosedPipe
	}
	q := url.Values{"session": {c.session}, "role": {c.role}, "token": {c.token}}
	req, err := http.NewRequest("POST", c.base+"/h/up?"+q.Encode(), bytes.NewReader(p))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("up chunk: relay returned %d", resp.StatusCode)
	}
	return len(p), nil
}

func (c *conn) Close() error {
	c.writeM.Lock()
	if c.closed {
		c.writeM.Unlock()
		return nil
	}
	c.closed = true
	c.writeM.Unlock()
	// Best-effort teardown signal to the relay.
	q := url.Values{"session": {c.session}, "token": {c.token}}
	req, _ := http.NewRequest("POST", c.base+"/h/close?"+q.Encode(), nil)
	if req != nil {
		if resp, err := c.hc.Do(req); err == nil {
			resp.Body.Close()
		}
	}
	return c.down.Close()
}

type addr struct{ s string }

func (a addr) Network() string { return "httpconn" }
func (a addr) String() string  { return a.s }

func (c *conn) LocalAddr() net.Addr  { return addr{"httpconn-local"} }
func (c *conn) RemoteAddr() net.Addr { return addr{c.base} }

// Deadlines are reported as satisfied (no-op): the HTTP body readers/writers do
// not support deadlines, and handshake cancellation is driven by context +
// Close instead.
func (c *conn) SetDeadline(t time.Time) error      { return nil }
func (c *conn) SetReadDeadline(t time.Time) error  { return nil }
func (c *conn) SetWriteDeadline(t time.Time) error { return nil }
