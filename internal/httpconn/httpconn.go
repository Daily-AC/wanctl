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
//	down: GET  /h/down?session=&role=&ack= — long-poll: returns available bytes
//	      (200), 204 if none within the poll window (client re-polls), or 410 when
//	      the session is closed (-> io.EOF).
//
// The down direction is acknowledged. Every data-bearing 200 carries a
// monotonic X-Wanctl-Down-Seq, and the next poll reports the highest sequence
// the reader has fully received in ack=. The relay holds a delivered chunk
// until it is acked and re-sends it otherwise, because an HTTP response that
// only half arrives would otherwise punch a hole in the middle of the
// end-to-end TLS stream — which the TLS layer reports, several minutes into a
// slow push, as "tls: bad record MAC" (issue #57).
package httpconn

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"wanctl/internal/admission"
	"wanctl/internal/config"
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
	ackSeq   uint64 // highest down-poll sequence fully received

	writeM     sync.Mutex
	pending    []byte
	writeErr   error
	flushTimer *time.Timer
	flushGen   uint64
	closed     bool
}

const (
	writeBatchBytes = 256 << 10
	writeFlushDelay = 5 * time.Millisecond

	// DownSeqHeader carries the sequence number of a data-bearing down-poll
	// response; DownAckParam is the query parameter the next poll reports the
	// last fully received sequence in. A poll without DownAckParam is a
	// pre-acknowledgement client and the relay serves it the old way.
	DownSeqHeader = "X-Wanctl-Down-Seq"
	DownAckParam  = "ack"

	// downPollAttempts bounds how many times one Read retries a down poll that
	// the carrier failed to deliver. The relay still holds the chunk, so a
	// retry is a re-send rather than a hole; the bound is what stops a
	// permanently broken link from spinning forever.
	downPollAttempts = 6
	downRetryDelay   = 250 * time.Millisecond
)

// Dial constructs a net.Conn for a session/role. base is the relay's HTTP origin
// (http:// or https://, or ws(s):// which is normalized). No network I/O happens
// here; the first Read long-polls the down channel.
func Dial(ctx context.Context, base, session, role, token string) (net.Conn, error) {
	return DialWith(ctx, base, session, role, token, nil)
}

// DialWith is Dial with an explicit *http.Client for the up/down requests. A nil
// client uses the package default. Tests use it to inject a carrier that drops,
// delays or truncates responses.
func DialWith(ctx context.Context, base, session, role, token string, hc *http.Client) (net.Conn, error) {
	httpBase, err := config.RelayHTTPOrigin(base)
	if err != nil {
		return nil, err
	}
	if hc == nil {
		hc = defaultClient()
	}
	return &conn{
		base:    httpBase,
		session: session,
		role:    role,
		token:   token,
		hc:      hc,
	}, nil
}

func defaultClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// A down poll parks on the relay for its whole poll window before
	// answering, so only the wait for response *headers* can be bounded
	// tightly. The previous blanket http.Client.Timeout bounded the entire
	// exchange, so on a slow link it fired while a response body was still
	// downloading and cut the body in half (issue #57).
	tr.ResponseHeaderTimeout = 45 * time.Second
	return &http.Client{Transport: tr, Timeout: 5 * time.Minute}
}

func (c *conn) Read(p []byte) (int, error) {
	c.readM.Lock()
	defer c.readM.Unlock()
	if err := c.flushWrites(); err != nil {
		return 0, err
	}
	if len(c.leftover) > 0 {
		n := copy(p, c.leftover)
		c.leftover = c.leftover[n:]
		return n, nil
	}
	if c.eof {
		return 0, io.EOF
	}
	failures := 0
	// retry reports whether a carrier failure is worth another poll. The relay
	// keeps an unacked chunk, so polling again re-sends the same bytes instead
	// of leaving a gap in the stream.
	retry := func(err error) error {
		failures++
		if failures >= downPollAttempts {
			return fmt.Errorf("down poll failed %d times in a row: %w", failures, err)
		}
		time.Sleep(downRetryDelay)
		return nil
	}
	for {
		if c.isClosed() {
			return 0, io.EOF
		}
		q := url.Values{
			"session":    {c.session},
			"role":       {c.role},
			DownAckParam: {strconv.FormatUint(c.ackSeq, 10)},
		}
		req, err := http.NewRequest("GET", c.base+"/h/down?"+q.Encode(), nil)
		if err != nil {
			return 0, err
		}
		admission.SetBearer(req, c.token)
		resp, err := c.hc.Do(req)
		if err != nil {
			if c.isClosed() {
				return 0, io.EOF
			}
			if giveUp := retry(err); giveUp != nil {
				return 0, giveUp
			}
			continue
		}
		switch resp.StatusCode {
		case http.StatusNoContent:
			resp.Body.Close()
			failures = 0
			continue // no data this round; poll again
		case http.StatusOK:
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				// The body was cut short. Do not advance the ack and do not
				// hand the partial body on: the relay re-sends the whole
				// chunk on the next poll. Accepting a truncated body here is
				// what made a multi-minute push die with "tls: bad record
				// MAC" (issue #57).
				if giveUp := retry(readErr); giveUp != nil {
					return 0, giveUp
				}
				continue
			}
			failures = 0
			if seq, convErr := strconv.ParseUint(resp.Header.Get(DownSeqHeader), 10, 64); convErr == nil && seq > 0 {
				if seq <= c.ackSeq {
					continue // a re-send of a chunk already consumed
				}
				c.ackSeq = seq
			}
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
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	c.pending = append(c.pending, p...)
	for len(c.pending) >= writeBatchBytes {
		if err := c.postPendingLocked(writeBatchBytes); err != nil {
			c.writeErr = err
			return 0, err
		}
	}
	if len(c.pending) > 0 && c.flushTimer == nil {
		c.flushGen++
		gen := c.flushGen
		c.flushTimer = time.AfterFunc(writeFlushDelay, func() { c.flushTimerFired(gen) })
	}
	return len(p), nil
}

func (c *conn) flushTimerFired(gen uint64) {
	c.writeM.Lock()
	defer c.writeM.Unlock()
	if c.flushTimer == nil || c.flushGen != gen {
		return
	}
	c.flushTimer = nil
	if c.closed || c.writeErr != nil || len(c.pending) == 0 {
		return
	}
	if err := c.postPendingLocked(len(c.pending)); err != nil {
		c.writeErr = err
	}
}

func (c *conn) flushWrites() error {
	c.writeM.Lock()
	defer c.writeM.Unlock()
	if c.writeErr != nil {
		return c.writeErr
	}
	c.stopFlushTimerLocked()
	if len(c.pending) == 0 {
		return nil
	}
	if err := c.postPendingLocked(len(c.pending)); err != nil {
		c.writeErr = err
		return err
	}
	return nil
}

func (c *conn) stopFlushTimerLocked() {
	if c.flushTimer != nil {
		c.flushTimer.Stop()
		c.flushTimer = nil
		c.flushGen++
	}
}

func (c *conn) postPendingLocked(n int) error {
	data := c.pending[:n]
	q := url.Values{"session": {c.session}, "role": {c.role}}
	req, err := http.NewRequest("POST", c.base+"/h/up?"+q.Encode(), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	admission.SetBearer(req, c.token)
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("up chunk: relay returned %d", resp.StatusCode)
	}
	copy(c.pending, c.pending[n:])
	c.pending = c.pending[:len(c.pending)-n]
	return nil
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
	c.stopFlushTimerLocked()
	flushErr := c.writeErr
	if flushErr == nil && len(c.pending) > 0 {
		flushErr = c.postPendingLocked(len(c.pending))
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
	return flushErr
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
