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
//
// Re-polling after a response the carrier failed to deliver is safe only
// against a relay that does hold that chunk. A relay from before this protocol
// dequeues before it answers, so the same retry would resume at the chunk after
// the lost bytes and skip them silently. The relay therefore marks every
// /h/down answer with X-Wanctl-Down-Ack, and a reader that has not seen it on
// this session fails the read instead of retrying.
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
	"sync/atomic"
	"time"

	"wanctl/internal/admission"
	"wanctl/internal/config"
	"wanctl/internal/limits"
	"wanctl/internal/relayhttp"
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
	ackable  bool   // the relay has answered this session with the ack protocol

	writeM     sync.Mutex
	pending    []byte
	flushTimer *time.Timer
	flushGen   uint64
	closed     bool
	upSeq      uint64 // sequence of the last /h/up handed out

	// upOrdered is set once the relay has said it orders sequenced writes.
	// Until then uploads go one at a time, as every older relay requires.
	upOrdered atomic.Bool
	upSlots   chan struct{} // one token per /h/up allowed in flight
	upWG      sync.WaitGroup

	errM     sync.Mutex
	writeErr error // first upload failure; every later Write and Read reports it
}

const (
	// writeBatchBytes is as much as the relay accepts in one /h/up. Upload is
	// one request at a time, so it cannot move more than a batch per round
	// trip: at the 0.5 s round trip of a CDN edge on another continent, 256 KiB
	// batches capped a push at about 400 KiB/s whatever the link could carry.
	writeBatchBytes = int(limits.RelayHTTPUploadBytes)
	writeFlushDelay = 5 * time.Millisecond

	// upWindow is how many /h/up a writer keeps in flight once the relay
	// orders them. One at a time moved a batch per round trip, about 1 MiB/s
	// at the round trip of a CDN edge on another continent.
	upWindow = 4
	// upAttempts bounds retries of one /h/up. Retrying is safe only against a
	// relay that orders writes, because it recognises the repeat by its
	// sequence and does not queue it twice.
	upAttempts   = 4
	upRetryDelay = 250 * time.Millisecond

	// UpSeqParam carries a write's place in its direction's stream, starting
	// at 1. UpSeqCapabilityHeader is how a relay says it holds out-of-order
	// writes until the gap fills and drops repeats.
	UpSeqParam            = "seq"
	UpSeqCapabilityHeader = "X-Wanctl-Up-Seq"

	// DownSeqHeader carries the sequence number of a data-bearing down-poll
	// response; DownAckParam is the query parameter the next poll reports the
	// last fully received sequence in. A poll without DownAckParam is a
	// pre-acknowledgement client and the relay serves it the old way.
	DownSeqHeader = "X-Wanctl-Down-Seq"
	DownAckParam  = "ack"

	// DownAckCapabilityHeader is how a relay says it holds a delivered chunk
	// until it is acked. A relay without it dequeues before it answers, so a
	// poll it failed to deliver is bytes that no longer exist anywhere and
	// re-polling would resume at the chunk after them. Retrying is therefore
	// gated on having seen this (or a sequence header) on this session.
	DownAckCapabilityHeader = "X-Wanctl-Down-Ack"

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
		upSlots: make(chan struct{}, upWindow),
	}, nil
}

func defaultClient() *http.Client {
	// The shared relay transport bounds the wait for response headers; the
	// whole request still has a bound, generous enough that a full
	// maxDrainBytes response downloads well inside it on the 60 KB/s link from
	// issue #57 (about 34 s) even after the poll parked on the relay first.
	return &http.Client{Transport: relayhttp.Shared(), Timeout: 5 * time.Minute}
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
	// retry reports whether a carrier failure is worth another poll. It is
	// worth one only against a relay that has shown it holds the chunk until
	// it is acked; against any other, polling again resumes after bytes that
	// are already gone, which is a silent hole in the stream rather than the
	// loud failure the caller needs. Pre-acknowledgement relays therefore keep
	// the old behaviour: an undelivered poll fails the read.
	retry := func(err error) error {
		if !c.ackable {
			return fmt.Errorf("down poll failed and cannot be retried: %w (this relay has not answered with %s, so it does not hold undelivered bytes)", err, DownAckCapabilityHeader)
		}
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
		// An upload still in flight when this Read started may fail while it
		// polls. The peer then never sees the bytes this read is waiting for
		// an answer to, so the failure has to end the wait.
		if err := c.uploadErr(); err != nil {
			return 0, err
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
		// Headers arrive before the body, so a response whose body is cut short
		// still proves what the relay speaks.
		if resp.Header.Get(DownAckCapabilityHeader) == "1" {
			c.ackable = true
		}
		seq, seqErr := strconv.ParseUint(resp.Header.Get(DownSeqHeader), 10, 64)
		if seqErr == nil && seq > 0 {
			c.ackable = true
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
			if seqErr == nil && seq > 0 {
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
	if err := c.uploadErr(); err != nil {
		return 0, err
	}
	c.pending = append(c.pending, p...)
	if len(c.pending) >= writeBatchBytes {
		// The timer was armed for bytes that are about to go out in this
		// batch. Left running, it fires while the batch is in flight and then
		// posts whatever tail the batch left behind as a request of its own,
		// which doubled the round trips of every bulk transfer.
		c.stopFlushTimerLocked()
	}
	for len(c.pending) >= writeBatchBytes {
		if err := c.sendLocked(writeBatchBytes); err != nil {
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
	if c.closed || c.uploadErr() != nil || len(c.pending) == 0 {
		return
	}
	c.sendLocked(len(c.pending))
}

// flushWrites hands every pending byte to an upload. Against a relay that
// orders writes it does not wait for them to land: the down poll that follows
// can run alongside, and the relay still queues the bytes in order.
func (c *conn) flushWrites() error {
	c.writeM.Lock()
	defer c.writeM.Unlock()
	if err := c.uploadErr(); err != nil {
		return err
	}
	c.stopFlushTimerLocked()
	if len(c.pending) == 0 {
		return nil
	}
	return c.sendLocked(len(c.pending))
}

func (c *conn) stopFlushTimerLocked() {
	if c.flushTimer != nil {
		c.flushTimer.Stop()
		c.flushTimer = nil
		c.flushGen++
	}
}

// sendLocked takes the first n pending bytes as the next write of this
// direction. It posts them in the background once the relay orders writes,
// waiting only for a free slot, and in the foreground otherwise.
func (c *conn) sendLocked(n int) error {
	data := make([]byte, n)
	copy(data, c.pending[:n])
	copy(c.pending, c.pending[n:])
	c.pending = c.pending[:len(c.pending)-n]
	c.upSeq++
	seq := c.upSeq
	if !c.upOrdered.Load() {
		if err := c.post(seq, data); err != nil {
			c.fail(err)
			return err
		}
		return nil
	}
	c.upSlots <- struct{}{}
	c.upWG.Add(1)
	go func() {
		defer c.upWG.Done()
		defer func() { <-c.upSlots }()
		if err := c.post(seq, data); err != nil {
			c.fail(err)
		}
	}()
	return nil
}

// post delivers one write. Every write names its sequence, which a relay that
// does not order writes ignores; retries happen only against one that does.
func (c *conn) post(seq uint64, data []byte) error {
	q := url.Values{"session": {c.session}, "role": {c.role}, UpSeqParam: {strconv.FormatUint(seq, 10)}}
	var lastErr error
	for attempt := 1; attempt <= upAttempts; attempt++ {
		if attempt > 1 {
			if !c.upOrdered.Load() {
				break
			}
			time.Sleep(time.Duration(attempt-1) * upRetryDelay)
		}
		req, err := http.NewRequest("POST", c.base+"/h/up?"+q.Encode(), bytes.NewReader(data))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		admission.SetBearer(req, c.token)
		resp, err := c.hc.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.Header.Get(UpSeqCapabilityHeader) == "1" {
			c.upOrdered.Store(true)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusOK:
			return nil
		case resp.StatusCode >= 500:
			lastErr = fmt.Errorf("up chunk: relay returned %d", resp.StatusCode)
			continue
		default:
			// 4xx is the relay refusing the write, not the carrier losing it.
			return fmt.Errorf("up chunk: relay returned %d", resp.StatusCode)
		}
	}
	return lastErr
}

func (c *conn) fail(err error) {
	c.errM.Lock()
	defer c.errM.Unlock()
	if c.writeErr == nil {
		c.writeErr = err
	}
}

func (c *conn) uploadErr() error {
	c.errM.Lock()
	defer c.errM.Unlock()
	return c.writeErr
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
	q := url.Values{"session": {c.session}, "role": {c.role}}
	var tail []byte
	if c.uploadErr() == nil && len(c.pending) > 0 {
		if c.upOrdered.Load() {
			// An ordering relay takes the last bytes with the close itself.
			tail = append([]byte(nil), c.pending...)
			c.pending = c.pending[:0]
			c.upSeq++
			q.Set(UpSeqParam, strconv.FormatUint(c.upSeq, 10))
		} else {
			c.sendLocked(len(c.pending))
		}
	}
	c.closed = true
	c.writeM.Unlock()
	// The last writes must land before the close, or the relay would end the
	// session with the tail of the stream still in flight.
	c.upWG.Wait()
	req, _ := http.NewRequest("POST", c.base+"/h/close?"+q.Encode(), bytes.NewReader(tail))
	if req != nil {
		admission.SetBearer(req, c.token)
		resp, err := c.hc.Do(req)
		switch {
		case err != nil && tail != nil:
			c.fail(err)
		case err == nil:
			if resp.StatusCode != http.StatusOK && tail != nil {
				c.fail(fmt.Errorf("close with final bytes: relay returned %d", resp.StatusCode))
			}
			resp.Body.Close()
		}
	}
	return c.uploadErr()
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
