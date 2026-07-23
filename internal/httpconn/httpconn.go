// Package httpconn provides a net.Conn that tunnels bytes over plain HTTP, so
// the end-to-end TLS handshake and framed protocol can run through any reverse
// proxy — including ones (like thunderbox's nginx edge) that strip the WebSocket
// Upgrade header AND buffer streaming responses. It is the proxy-agnostic
// alternative to wsconn.
//
// A session uses two HTTP request shapes against the relay, both finite
// request/response pairs (no upgrade, no streaming body) so a buffering proxy
// forwards them promptly:
//
//	up:   POST /h/up?session=&role=     — one request per Write, body = bytes.
//	down: GET  /h/down?session=&role=   — long-poll: returns available bytes
//	      (200), 204 if none within the poll window (client re-polls), or 410 when
//	      the session is closed (-> io.EOF).
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

	"wanctl/internal/admission"
)

type conn struct {
	base    string // http(s)://host
	session string
	role    string
	token   string
	hc      *http.Client

	readM    sync.Mutex
	leftover []byte
	eof      bool

	writeM sync.Mutex
	closed bool
}

// Dial constructs a net.Conn for a session/role. base is the relay's HTTP origin
// (http:// or https://, or ws(s):// which is normalized). No network I/O happens
// here; the first Read long-polls the down channel.
func Dial(ctx context.Context, base, session, role, token string) (net.Conn, error) {
	return &conn{
		base:    normalizeBase(base),
		session: session,
		role:    role,
		token:   token,
		hc:      &http.Client{Timeout: 60 * time.Second},
	}, nil
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

func (c *conn) Read(p []byte) (int, error) {
	c.readM.Lock()
	defer c.readM.Unlock()
	if len(c.leftover) > 0 {
		n := copy(p, c.leftover)
		c.leftover = c.leftover[n:]
		return n, nil
	}
	if c.eof {
		return 0, io.EOF
	}
	q := url.Values{"session": {c.session}, "role": {c.role}}
	downURL := c.base + "/h/down?" + q.Encode()
	for {
		if c.isClosed() {
			return 0, io.EOF
		}
		req, err := http.NewRequest("GET", downURL, nil)
		if err != nil {
			return 0, err
		}
		admission.SetBearer(req, c.token)
		resp, err := c.hc.Do(req)
		if err != nil {
			if c.isClosed() {
				return 0, io.EOF
			}
			return 0, err
		}
		switch resp.StatusCode {
		case http.StatusNoContent:
			resp.Body.Close()
			continue // no data this round; poll again
		case http.StatusOK:
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if len(body) == 0 {
				continue
			}
			n := copy(p, body)
			if n < len(body) {
				c.leftover = body[n:]
			}
			return n, nil
		case http.StatusGone, http.StatusNotFound:
			resp.Body.Close()
			c.eof = true
			return 0, io.EOF
		default:
			resp.Body.Close()
			return 0, fmt.Errorf("down poll: relay returned %d", resp.StatusCode)
		}
	}
}

func (c *conn) Write(p []byte) (int, error) {
	c.writeM.Lock()
	defer c.writeM.Unlock()
	if c.closed {
		return 0, io.ErrClosedPipe
	}
	q := url.Values{"session": {c.session}, "role": {c.role}}
	req, err := http.NewRequest("POST", c.base+"/h/up?"+q.Encode(), bytes.NewReader(p))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	admission.SetBearer(req, c.token)
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

func (c *conn) isClosed() bool {
	c.writeM.Lock()
	defer c.writeM.Unlock()
	return c.closed
}

func (c *conn) Close() error {
	c.writeM.Lock()
	if c.closed {
		c.writeM.Unlock()
		return nil
	}
	c.closed = true
	c.writeM.Unlock()
	q := url.Values{"session": {c.session}}
	req, _ := http.NewRequest("POST", c.base+"/h/close?"+q.Encode(), nil)
	if req != nil {
		admission.SetBearer(req, c.token)
		if resp, err := c.hc.Do(req); err == nil {
			resp.Body.Close()
		}
	}
	return nil
}

type addr struct{ s string }

func (a addr) Network() string { return "httpconn" }
func (a addr) String() string  { return a.s }

func (c *conn) LocalAddr() net.Addr  { return addr{"httpconn-local"} }
func (c *conn) RemoteAddr() net.Addr { return addr{c.base} }

// Deadlines are no-ops: requests carry their own client timeout and handshake
// cancellation is driven by Close.
func (c *conn) SetDeadline(t time.Time) error      { return nil }
func (c *conn) SetReadDeadline(t time.Time) error  { return nil }
func (c *conn) SetWriteDeadline(t time.Time) error { return nil }
