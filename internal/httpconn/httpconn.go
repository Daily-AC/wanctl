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
	base        string // http(s)://host
	session     string
	role        string
	token       string
	hc          *http.Client
	laneClients [DownWindowSize]*http.Client
	laneMu      sync.Mutex
	laneBusy    [2][DownWindowSize]int // upload, download; lane 0 can multiplex small uploads
	laneWake    chan struct{}

	readM       sync.Mutex
	leftover    []byte
	eof         bool
	ackSeq      uint64 // highest down-poll sequence fully received
	ackable     bool   // the relay has answered this session with the ack protocol
	downMax     int    // bytes this reader asks one down poll to carry
	downWindow  atomic.Bool
	batch       *downBatch
	prefetchWG  sync.WaitGroup
	closeCtx    context.Context
	closeCancel context.CancelFunc

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
	DownWindowSize = 4
	// writeBatchBytes is as much as the relay accepts in one /h/up. Upload is
	// one request at a time, so it cannot move more than a batch per round
	// trip: at the 0.5 s round trip of a CDN edge on another continent, 256 KiB
	// batches capped a push at about 400 KiB/s whatever the link could carry.
	writeBatchBytes = int(limits.RelayHTTPUploadBytes)
	writeFlushDelay = 5 * time.Millisecond

	// upWindow is how many /h/up a writer keeps in flight once the relay
	// orders them. One at a time moved a batch per round trip, about 1 MiB/s
	// at the round trip of a CDN edge on another continent.
	upWindow = 2 * DownWindowSize
	// upAttempts bounds retries of one /h/up. Retrying is safe only against a
	// relay that orders writes, because it recognises the repeat by its
	// sequence and does not queue it twice.
	upAttempts   = 4
	upRetryDelay = 250 * time.Millisecond

	// UpSeqParam carries a write's place in its direction's stream, starting
	// at 1. UpSeqCapabilityHeader is how a relay says it holds out-of-order
	// writes until the gap fills and drops repeats.
	UpSeqParam                 = "seq"
	UpSeqCapabilityHeader      = "X-Wanctl-Up-Seq"
	DownWindowCapabilityHeader = "X-Wanctl-Down-Window"
	DownWantParam              = "want"

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

	// DownMaxParam is the most bytes the reader wants in one down-poll
	// response. A relay that does not know it answers with its own cap.
	DownMaxParam = "max"

	// downPollAttempts bounds how many times one Read retries a down poll that
	// the carrier failed to deliver. The relay still holds the chunk, so a
	// retry is a re-send rather than a hole; the bound is what stops a
	// permanently broken link from spinning forever.
	downPollAttempts = 6
	downRetryDelay   = 250 * time.Millisecond

	// A reader starts by asking for the chunk every relay has always served
	// and doubles it while full chunks keep arriving quickly: each poll costs
	// a round trip and a fresh HTTP stream, so bigger chunks move bulk data
	// several times faster on a CDN path. A chunk that takes long to arrive
	// halves it again, so one response stays well inside the client's
	// five-minute request bound on a slow link.
	downMaxFloor   = 2 << 20
	downMaxCeiling = 16 << 20
	downGrowWithin = 4 * time.Second
)

// downShrinkPast is how long a chunk may take before the next one is halved;
// a variable so tests need not wait it out.
var downShrinkPast = 30 * time.Second

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
	explicit := hc != nil
	if hc == nil {
		hc = defaultClient()
	}
	c := &conn{
		base:     httpBase,
		session:  session,
		role:     role,
		token:    token,
		hc:       hc,
		laneWake: make(chan struct{}),
		upSlots:  make(chan struct{}, upWindow),
		downMax:  downMaxFloor,
	}
	c.closeCtx, c.closeCancel = context.WithCancel(context.Background())
	c.laneClients[0] = hc
	if explicit {
		for i := 1; i < DownWindowSize; i++ {
			c.laneClients[i] = hc
		}
	}
	return c, nil
}

// MarkOrdered records that the relay announced write ordering before the
// session carried a byte, on /h/dial or /h/poll, so the first write can go out
// without waiting for an /h/up answer to say so. It is a no-op on any other
// net.Conn.
func MarkOrdered(nc net.Conn) {
	if c, ok := nc.(*conn); ok {
		c.upOrdered.Store(true)
	}
}

// MarkWindow records the download-window capability carried by /h/dial or
// /h/poll, before the first /h/down response can advertise it itself.
func MarkWindow(nc net.Conn) {
	if c, ok := nc.(*conn); ok {
		c.downWindow.Store(true)
	}
}

func defaultClient() *http.Client {
	// The shared relay transport bounds the wait for response headers; the
	// whole request still has a bound, generous enough that a full
	// maxDrainBytes response downloads well inside it on the 60 KB/s link from
	// issue #57 (about 34 s) even after the poll parked on the relay first.
	return &http.Client{Transport: relayhttp.Shared(), Timeout: 5 * time.Minute}
}

var sharedLanes struct {
	sync.Mutex
	clients [DownWindowSize]*http.Client
}

func processLane(i int) *http.Client {
	sharedLanes.Lock()
	defer sharedLanes.Unlock()
	if sharedLanes.clients[i] == nil {
		transport := http.RoundTripper(relayhttp.Shared())
		if i != 0 {
			transport = relayhttp.New(nil)
		}
		sharedLanes.clients[i] = &http.Client{Transport: transport, Timeout: 5 * time.Minute}
	}
	return sharedLanes.clients[i]
}

func (c *conn) laneClient(i int) *http.Client {
	c.laneMu.Lock()
	defer c.laneMu.Unlock()
	if c.laneClients[i] == nil {
		c.laneClients[i] = processLane(i)
	}
	return c.laneClients[i]
}

func (c *conn) acquireLane(direction, start int) int {
	for {
		c.laneMu.Lock()
		for j := 0; j < DownWindowSize; j++ {
			i := (start + j) % DownWindowSize
			if c.laneBusy[direction][i] == 0 {
				c.laneBusy[direction][i] = 1
				c.laneMu.Unlock()
				return i
			}
		}
		wake := c.laneWake
		c.laneMu.Unlock()
		<-wake
	}
}

func (c *conn) reserveLaneZero(direction int) {
	c.laneMu.Lock()
	c.laneBusy[direction][0]++
	c.laneMu.Unlock()
}

func (c *conn) releaseLane(direction, i int) {
	c.laneMu.Lock()
	c.laneBusy[direction][i]--
	close(c.laneWake)
	c.laneWake = make(chan struct{})
	c.laneMu.Unlock()
}

func (c *conn) closeFailedLane(i int) {
	if tr, ok := c.laneClient(i).Transport.(interface{ CloseIdleConnections() }); ok {
		tr.CloseIdleConnections()
	}
}

type downResult struct {
	want    uint64
	max     int
	data    []byte
	seq     uint64
	status  int
	ackable bool
	window  bool
	retried bool // a response body was cut short before this complete result
	took    time.Duration
	err     error
}

type downBatch struct {
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	results   chan downResult
	pending   map[uint64]downResult
	next, end uint64
}

// downPoll retries the same numbered chunk on a different lane after a
// carrier failure. A poll without want keeps the legacy single-request shape.
func (c *conn) downPoll(ctx context.Context, ack, want uint64, limit int, retryable bool) downResult {
	nextLane := 0
	hadBodyFailure := false
	for attempt := 1; attempt <= downPollAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return downResult{want: want, err: err}
		}
		q := url.Values{"session": {c.session}, "role": {c.role}, DownAckParam: {strconv.FormatUint(ack, 10)}, DownMaxParam: {strconv.Itoa(limit)}}
		if want != 0 {
			q.Set(DownWantParam, strconv.FormatUint(want, 10))
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/h/down?"+q.Encode(), nil)
		if err != nil {
			return downResult{want: want, err: err}
		}
		admission.SetBearer(req, c.token)
		lane := 0
		if want != 0 {
			lane = c.acquireLane(1, nextLane)
		}
		resp, err := c.laneClient(lane).Do(req)
		result := downResult{want: want, max: limit}
		if err == nil {
			result.status = resp.StatusCode
			result.ackable = resp.Header.Get(DownAckCapabilityHeader) == "1"
			result.window = resp.Header.Get(DownWindowCapabilityHeader) == "4"
			result.seq, _ = strconv.ParseUint(resp.Header.Get(DownSeqHeader), 10, 64)
			if result.seq != 0 {
				result.ackable = true
			}
			if resp.StatusCode == http.StatusOK {
				startedBody := time.Now()
				result.data, err = io.ReadAll(resp.Body)
				result.took = time.Since(startedBody)
			}
			resp.Body.Close()
		}
		if want != 0 {
			c.releaseLane(1, lane)
		}
		if err == nil {
			result.retried = hadBodyFailure
			return result
		}
		if result.status == http.StatusOK {
			hadBodyFailure = true
		}
		c.closeFailedLane(lane)
		if ctx.Err() != nil {
			return downResult{want: want, err: ctx.Err()}
		}
		if !retryable && !result.ackable {
			return downResult{want: want, err: fmt.Errorf("down poll failed and cannot be retried: %w (this relay has not answered with %s, so it does not hold undelivered bytes)", err, DownAckCapabilityHeader)}
		}
		if attempt == downPollAttempts {
			return downResult{want: want, err: fmt.Errorf("down poll failed %d times in a row: %w", attempt, err)}
		}
		retryable = true
		nextLane = (lane + 1) % DownWindowSize
		select {
		case <-time.After(downRetryDelay):
		case <-ctx.Done():
			return downResult{want: want, err: ctx.Err()}
		}
	}
	panic("unreachable")
}

func (c *conn) startBatch() {
	ctx, cancel := context.WithCancel(c.closeCtx)
	b := &downBatch{ctx: ctx, cancel: cancel, results: make(chan downResult, DownWindowSize), pending: make(map[uint64]downResult), next: c.ackSeq + 1, end: c.ackSeq + DownWindowSize}
	c.batch = b
	ack := c.ackSeq
	for want := b.next; want <= b.end; want++ {
		c.launchPoll(b, ack, want)
	}
}

func (c *conn) launchPoll(b *downBatch, ack, want uint64) {
	limit := min(c.downMax, 4<<20)
	c.prefetchWG.Add(1)
	b.wg.Add(1)
	go func() {
		defer c.prefetchWG.Done()
		defer b.wg.Done()
		b.results <- c.downPoll(b.ctx, ack, want, limit, true)
	}()
}

func (c *conn) extendBatch() {
	b := c.batch
	b.end++
	c.launchPoll(b, c.ackSeq, b.end)
}

func (c *conn) stopBatch() {
	if c.batch == nil {
		return
	}
	b := c.batch
	b.cancel()
	b.wg.Wait()
	c.batch = nil
}

func (c *conn) nextBatchResult() downResult {
	b := c.batch
	for {
		if r, ok := b.pending[b.next]; ok {
			delete(b.pending, b.next)
			b.next++
			return r
		}
		select {
		case r := <-b.results:
			b.pending[r.want] = r
		case <-c.closeCtx.Done():
			return downResult{err: io.EOF}
		}
	}
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
	for {
		if c.isClosed() {
			return 0, io.EOF
		}
		if err := c.uploadErr(); err != nil {
			return 0, err
		}
		var r downResult
		windowed := c.batch != nil
		if windowed {
			r = c.nextBatchResult()
		} else {
			r = c.downPoll(c.closeCtx, c.ackSeq, 0, c.downMax, c.ackable)
		}
		if r.err != nil {
			if windowed {
				c.stopBatch()
			}
			if c.isClosed() {
				return 0, io.EOF
			}
			return 0, r.err
		}
		if r.ackable {
			c.ackable = true
		}
		if r.window {
			c.downWindow.Store(true)
		}
		switch r.status {
		case http.StatusNoContent:
			if windowed {
				c.stopBatch()
			}
			continue
		case http.StatusOK:
			if windowed && r.seq != r.want {
				c.stopBatch()
				return 0, fmt.Errorf("down poll: want %d received sequence %d", r.want, r.seq)
			}
			if r.retried {
				c.adjustDownMax(len(r.data), r.took, io.ErrUnexpectedEOF)
			} else {
				c.adjustDownMax(len(r.data), r.took, nil)
			}
			if r.seq > 0 {
				if r.seq <= c.ackSeq {
					continue
				}
				c.ackSeq = r.seq
			}
			if windowed && len(r.data) < r.max && c.batch != nil {
				c.stopBatch()
			}
			if !windowed && c.downWindow.Load() && len(r.data) == r.max {
				c.startBatch()
			} else if windowed && c.batch != nil && len(r.data) == r.max {
				c.extendBatch()
			}
			if len(r.data) == 0 {
				continue
			}
			n := copy(p, r.data)
			if n < len(r.data) {
				c.leftover = r.data[n:]
			}
			return n, nil
		case http.StatusGone, http.StatusNotFound:
			if windowed {
				c.stopBatch()
			}
			c.eof = true
			return 0, io.EOF
		default:
			if windowed {
				c.stopBatch()
			}
			return 0, fmt.Errorf("down poll: relay returned %d", r.status)
		}
	}
}

// adjustDownMax sizes the next poll's chunk from how the last one arrived.
func (c *conn) adjustDownMax(n int, took time.Duration, readErr error) {
	switch {
	case readErr != nil || took > downShrinkPast:
		c.downMax = max(c.downMax/2, downMaxFloor)
	case n >= c.downMax && took < downGrowWithin:
		c.downMax = min(c.downMax*2, downMaxCeiling)
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
	nextLane := 0
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
		ordered := c.upOrdered.Load()
		// Small requests share the already warm HTTP/2 lane, even when the
		// handshake and command pipeline puts several of them in flight.
		// Only full upload batches justify opening another relay connection.
		striped := ordered && len(data) == writeBatchBytes
		lane := 0
		if striped {
			lane = c.acquireLane(0, nextLane)
		} else {
			c.reserveLaneZero(0)
		}
		client := c.laneClient(lane)
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			c.closeFailedLane(lane)
			c.releaseLane(0, lane)
			nextLane = (lane + 1) % DownWindowSize
			continue
		}
		if resp.Header.Get(UpSeqCapabilityHeader) == "1" {
			c.upOrdered.Store(true)
		}
		_, bodyErr := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		c.releaseLane(0, lane)
		if bodyErr != nil {
			lastErr = bodyErr
			c.closeFailedLane(lane)
			nextLane = (lane + 1) % DownWindowSize
			continue
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			return nil
		case resp.StatusCode >= 500:
			lastErr = fmt.Errorf("up chunk: relay returned %d", resp.StatusCode)
			c.closeFailedLane(lane)
			nextLane = (lane + 1) % DownWindowSize
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
	c.closeCancel()
	// A Read may have passed its closed check and still be starting a batch.
	// Waiting for readM makes every prefetchWG.Add happen before Wait.
	c.readM.Lock()
	c.readM.Unlock()
	c.prefetchWG.Wait()
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
