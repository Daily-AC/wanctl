package httpconn

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type failingLane struct {
	base                            http.RoundTripper
	id                              int
	mu                              sync.Mutex
	ups, downs, drops, cuts, closed int
	wants                           int // downs that were windowed polls
}

func (l *failingLane) RoundTrip(req *http.Request) (*http.Response, error) {
	l.mu.Lock()
	if req.URL.Path == "/h/up" {
		l.ups++
	}
	if req.URL.Path == "/h/down" {
		l.downs++
		if req.URL.Query().Has(DownWantParam) {
			l.wants++
		}
	}
	l.mu.Unlock()
	resp, err := l.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.id == 2 && req.URL.Path == "/h/up" && l.drops == 0 {
		l.drops++
		resp.Body.Close()
		return nil, errors.New("injected lost upload response")
	}
	if l.id == 3 && req.URL.Path == "/h/down" && req.URL.Query().Has(DownWantParam) && resp.StatusCode == http.StatusOK && l.cuts == 0 {
		l.cuts++
		resp.Body = &cutBody{inner: resp.Body, left: 1024}
	}
	return resp, nil
}

func (l *failingLane) CloseIdleConnections() { l.mu.Lock(); l.closed++; l.mu.Unlock() }

type cutBody struct {
	inner io.ReadCloser
	left  int
}

func (b *cutBody) Read(p []byte) (int, error) {
	if b.left == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	if len(p) > b.left {
		p = p[:b.left]
	}
	n, err := b.inner.Read(p)
	b.left -= n
	return n, err
}
func (b *cutBody) Close() error { return b.inner.Close() }

// windowRelay stands in for a relay that orders writes and serves payload
// through the download window: chunk K is cut once every chunk before it has
// been, whatever order the polls for them arrive in, and a retried poll gets
// the chunk it was assigned. Every /h/up is handed to up.
func windowRelay(payload []byte, up func(seq uint64, body []byte)) *httptest.Server {
	var mu sync.Mutex
	assigned := map[uint64][]byte{}
	next, offset := uint64(1), 0
	changed := make(chan struct{})
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/h/up":
			seq, _ := strconv.ParseUint(r.URL.Query().Get(UpSeqParam), 10, 64)
			body, _ := io.ReadAll(r.Body)
			up(seq, body)
			w.Header().Set(UpSeqCapabilityHeader, "1")
		case "/h/down":
			w.Header().Set(DownAckCapabilityHeader, "1")
			w.Header().Set(DownWindowCapabilityHeader, "4")
			want, _ := strconv.ParseUint(r.URL.Query().Get(DownWantParam), 10, 64)
			if want == 0 {
				ack, _ := strconv.ParseUint(r.URL.Query().Get(DownAckParam), 10, 64)
				want = ack + 1
			}
			max, _ := strconv.Atoi(r.URL.Query().Get(DownMaxParam))
			var chunk []byte
			for {
				mu.Lock()
				if c, ok := assigned[want]; ok {
					chunk = c
					mu.Unlock()
					break
				}
				if want == next {
					if offset < len(payload) {
						end := min(offset+max, len(payload))
						chunk = payload[offset:end]
						assigned[want] = chunk
						offset = end
						next++
						close(changed)
						changed = make(chan struct{})
					}
					mu.Unlock()
					break
				}
				wait := changed
				mu.Unlock()
				select {
				case <-wait:
				case <-r.Context().Done():
					return
				}
			}
			if chunk == nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.Header().Set(DownSeqHeader, strconv.FormatUint(want, 10))
			w.Header().Set("Content-Length", strconv.Itoa(len(chunk)))
			w.Write(chunk)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
}

func TestLaneFailuresReplay32MiB(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789abcdef"), 2<<20)
	var mu sync.Mutex
	uploads := map[uint64][]byte{}
	srv := windowRelay(payload, func(seq uint64, body []byte) {
		time.Sleep(10 * time.Millisecond) // keep four uploads in flight
		mu.Lock()
		uploads[seq] = body
		mu.Unlock()
	})
	defer srv.Close()
	nc, err := Dial(t.Context(), srv.URL, "s", "client", "tok")
	if err != nil {
		t.Fatal(err)
	}
	c := nc.(*conn)
	var lanes [laneCount]*failingLane
	for i := range lanes {
		lanes[i] = &failingLane{base: http.DefaultTransport, id: i}
		c.laneClients[i] = &http.Client{Transport: lanes[i]}
	}
	MarkOrdered(nc)
	MarkWindow(nc)
	if _, err := nc.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(nc, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("download mismatch")
	}
	if err := nc.Close(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	var uploaded []byte
	for seq := uint64(1); seq <= uint64(len(uploads)); seq++ {
		uploaded = append(uploaded, uploads[seq]...)
	}
	mu.Unlock()
	if !bytes.Equal(uploaded, payload) {
		t.Fatal("upload mismatch")
	}
	for i, l := range lanes {
		l.mu.Lock()
		ups, wants, drops, cuts, closed := l.ups, l.wants, l.drops, l.cuts, l.closed
		l.mu.Unlock()
		// Every write here is a full batch, and every download past the
		// first chunk a windowed poll: none of it belongs on lane 0.
		if i == 0 && (ups != 0 || wants != 0) {
			t.Errorf("lane 0 carried bulk: %d uploads, %d windowed polls", ups, wants)
		}
		if i > 0 && (ups == 0 || wants == 0) {
			t.Errorf("lane %d requests: up=%d windowed down=%d", i, ups, wants)
		}
		if i == 2 && (drops != 1 || closed == 0) {
			t.Errorf("lane 2: drops=%d closeIdle=%d", drops, closed)
		}
		if i == 3 && (cuts != 1 || closed == 0) {
			t.Errorf("lane 3: cuts=%d closeIdle=%d", cuts, closed)
		}
	}
}

func TestCloseStopsWindowPrefetch(t *testing.T) {
	var active atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/h/down" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set(DownAckCapabilityHeader, "1")
		w.Header().Set(DownWindowCapabilityHeader, "4")
		if !r.URL.Query().Has(DownWantParam) {
			w.Header().Set(DownSeqHeader, "1")
			w.Write(make([]byte, downMaxFloor))
			return
		}
		active.Add(1)
		defer active.Add(-1)
		<-r.Context().Done()
	}))
	defer srv.Close()
	nc, err := Dial(t.Context(), srv.URL, "s", "client", "tok")
	if err != nil {
		t.Fatal(err)
	}
	MarkWindow(nc)
	if _, err := nc.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for active.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if active.Load() == 0 {
		t.Fatal("prefetch never started")
	}
	done := make(chan error, 1)
	go func() { done <- nc.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not stop prefetch")
	}
	deadline = time.Now().Add(time.Second)
	for active.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := active.Load(); got != 0 {
		t.Fatalf("%d prefetched requests still active", got)
	}
}

func TestWindowPrefetchSlidesAfterEachFullChunk(t *testing.T) {
	six := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/h/down" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set(DownAckCapabilityHeader, "1")
		w.Header().Set(DownWindowCapabilityHeader, "4")
		want := r.URL.Query().Get(DownWantParam)
		if want == "" {
			w.Header().Set(DownSeqHeader, "1")
			w.Write(make([]byte, downMaxFloor))
			return
		}
		if want == "2" {
			w.Header().Set(DownSeqHeader, "2")
			w.Write(make([]byte, 4<<20))
			return
		}
		if want == "6" {
			select {
			case six <- struct{}{}:
			default:
			}
		}
		<-r.Context().Done()
	}))
	defer srv.Close()
	nc, err := Dial(t.Context(), srv.URL, "s", "client", "tok")
	if err != nil {
		t.Fatal(err)
	}
	MarkWindow(nc)
	defer nc.Close()
	if _, err := io.ReadFull(nc, make([]byte, downMaxFloor)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(nc, make([]byte, 4<<20)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-six:
	case <-time.After(time.Second):
		t.Fatal("want=6 was not launched while want=3..5 remained in flight")
	}
}

type countingTransport struct {
	base  http.RoundTripper
	mu    sync.Mutex
	ups   int
	downs int
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	switch r.URL.Path {
	case "/h/up":
		c.ups++
	case "/h/down":
		c.downs++
	}
	c.mu.Unlock()
	return c.base.RoundTrip(r)
}

func (c *countingTransport) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ups, c.downs
}

type pipelineTransport struct {
	started chan struct{}
	release <-chan struct{}
}

func (p *pipelineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/h/up" {
		io.Copy(io.Discard, r.Body)
		p.started <- struct{}{}
		<-p.release
	}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(nil)), Request: r}, nil
}

func TestBulkUploadUsesFourLanesNotLaneZero(t *testing.T) {
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	backend := &pipelineTransport{started: started, release: release}
	var counters [laneCount]*countingTransport
	for i := range counters {
		counters[i] = &countingTransport{base: backend}
	}
	nc, err := DialWith(t.Context(), "http://127.0.0.1", "s", "client", "tok", &http.Client{Transport: counters[0]})
	if err != nil {
		t.Fatal(err)
	}
	c := nc.(*conn)
	for i := range counters {
		c.laneClients[i] = &http.Client{Transport: counters[i]}
	}
	MarkOrdered(nc)
	if _, err := nc.Write(bytes.Repeat([]byte("x"), 8<<20)); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("bulk uploads did not reach four lanes")
		}
	}
	releaseOnce.Do(func() { close(release) })
	if err := nc.Close(); err != nil {
		t.Fatal(err)
	}
	for i, counter := range counters {
		up, _ := counter.counts()
		if i == 0 && up != 0 {
			t.Errorf("lane 0 received %d full batches", up)
		}
		if i > 0 && up == 0 {
			t.Errorf("lane %d received no uploads", i)
		}
	}
}

// A tiny push can overlap TLS, hello, file_put, and data uploads. Their
// concurrent requests must still stay on the already warm HTTP/2 lane.
func TestSmallPushPipelineStaysOnLaneZero(t *testing.T) {
	started := make(chan struct{}, 5)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	backend := &pipelineTransport{started: started, release: release}
	var counters [laneCount]*countingTransport
	for i := range counters {
		counters[i] = &countingTransport{base: backend}
	}
	nc, err := DialWith(t.Context(), "http://127.0.0.1", "s", "client", "tok", &http.Client{Transport: counters[0]})
	if err != nil {
		t.Fatal(err)
	}
	c := nc.(*conn)
	for i := range counters {
		c.laneClients[i] = &http.Client{Transport: counters[i]}
	}
	MarkOrdered(nc)
	for _, size := range []int{517, 160, 256, 1, 64 << 10} {
		if _, err := nc.Write(make([]byte, size)); err != nil {
			t.Fatal(err)
		}
		if err := c.flushWrites(); err != nil {
			t.Fatal(err)
		}
	}
	for range 4 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("pipeline uploads did not overlap")
		}
	}
	for i := 1; i < len(counters); i++ {
		if up, _ := counters[i].counts(); up != 0 {
			t.Errorf("small pipeline used lane %d for %d uploads", i, up)
		}
	}
	releaseOnce.Do(func() { close(release) })
	if err := nc.Close(); err != nil {
		t.Fatal(err)
	}
	if up, _ := counters[0].counts(); up != 5 {
		t.Fatalf("lane 0 uploaded %d chunks, want 5", up)
	}
}

type smallTrafficTransport struct{ wants atomic.Int32 }

func (s *smallTrafficTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	h := make(http.Header)
	var body []byte
	if r.URL.Path == "/h/down" {
		if r.URL.Query().Has(DownWantParam) {
			s.wants.Add(1)
		}
		h.Set(DownAckCapabilityHeader, "1")
		h.Set(DownWindowCapabilityHeader, "4")
		h.Set(DownSeqHeader, "1")
		body = bytes.Repeat([]byte("y"), 64<<10)
	} else if r.Body != nil {
		io.Copy(io.Discard, r.Body)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
}

func TestSmallTrafficStaysOnLaneZero(t *testing.T) {
	backend := &smallTrafficTransport{}
	var counters [laneCount]*countingTransport
	for i := range counters {
		counters[i] = &countingTransport{base: backend}
	}
	nc, err := DialWith(t.Context(), "http://127.0.0.1", "s", "client", "tok", &http.Client{Transport: counters[0]})
	if err != nil {
		t.Fatal(err)
	}
	c := nc.(*conn)
	for i := range counters {
		c.laneClients[i] = &http.Client{Transport: counters[i]}
	}
	MarkOrdered(nc)
	if _, err := nc.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(nc, make([]byte, 64<<10)); err != nil {
		t.Fatal(err)
	}
	if err := nc.Close(); err != nil {
		t.Fatal(err)
	}
	if got := backend.wants.Load(); got != 0 {
		t.Fatalf("64 KiB pull sent %d window polls", got)
	}
	for i, counter := range counters {
		up, down := counter.counts()
		if i == 0 && (up == 0 || down == 0) {
			t.Errorf("lane 0 counts = %d up, %d down", up, down)
		}
		if i > 0 && (up != 0 || down != 0) {
			t.Errorf("lane %d used for small traffic: %d up, %d down", i, up, down)
		}
	}
}

// slowLink models one relay connection on a slow link. A request's body
// crosses the up pipe and its response's body the down pipe, each after
// whatever this connection sent before it, as bytes do on one TCP connection;
// a long poll parked on the relay holds neither. It records what crossed and
// how much bulk every small request found queued ahead of it.
type slowLink struct {
	base     http.RoundTripper
	up, down linkPipe

	mu          sync.Mutex
	fullUps     int // full upload batches
	wants       int // windowed download polls
	queuedAhead []int
}

type linkPipe struct {
	rate   float64 // bytes per second
	mu     sync.Mutex
	queued atomic.Int64 // bytes waiting for or crossing the pipe
}

func (p *linkPipe) cross(n int) {
	p.queued.Add(int64(n))
	defer p.queued.Add(-int64(n))
	p.mu.Lock()
	defer p.mu.Unlock()
	time.Sleep(time.Duration(float64(n) / p.rate * float64(time.Second)))
}

func (l *slowLink) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	bulk := len(body) == writeBatchBytes || req.URL.Query().Has(DownWantParam)
	l.mu.Lock()
	if len(body) == writeBatchBytes {
		l.fullUps++
	}
	if req.URL.Query().Has(DownWantParam) {
		l.wants++
	}
	if !bulk {
		l.queuedAhead = append(l.queuedAhead, int(l.up.queued.Load()+l.down.queued.Load()))
	}
	l.mu.Unlock()
	l.up.cross(len(body) + 1)
	resp, err := l.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	l.down.cross(len(data) + 1)
	resp.Body = io.NopCloser(bytes.NewReader(data))
	return resp, nil
}

// A device serving a pull uploads in bulk, one receiving a push downloads in
// bulk, and its /h/poll and the handshakes of new sessions share lane 0 with
// both. On a slow uplink every one of them used to wait behind a 1 MiB batch
// or a windowed chunk on that lane: 8-22 s per hop of a new session, and a
// poll late enough for the relay to end every session the device held
// (2026-10-13). Bulk now keeps to the other lanes, so the poll and a second
// session's handshake find nothing queued ahead of them.
func TestPollAndHandshakesNeverQueueBehindBulk(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789abcdef"), 1<<20) // 16 MiB
	var mu sync.Mutex
	uploaded := 0
	srv := windowRelay(payload, func(_ uint64, body []byte) {
		mu.Lock()
		uploaded += len(body)
		mu.Unlock()
	})
	defer srv.Close()

	var links [laneCount]*slowLink
	clients := func(c *conn) {
		for i := range links {
			c.laneClients[i] = &http.Client{Transport: links[i]}
		}
	}
	for i := range links {
		links[i] = &slowLink{base: http.DefaultTransport, up: linkPipe{rate: 4 << 20}, down: linkPipe{rate: 8 << 20}}
	}
	// The agent's registration poll, which the relay answers at once here.
	poll := func() {
		resp, err := (&http.Client{Transport: links[0]}).Get(srv.URL + "/h/poll")
		if err != nil {
			t.Error(err)
			return
		}
		resp.Body.Close()
	}
	// A new session's handshake: a few hundred bytes on its own connection
	// object, sharing the process's lanes.
	handshake := func() {
		nc, err := Dial(t.Context(), srv.URL, "other", "agent", "tok")
		if err != nil {
			t.Fatal(err)
		}
		clients(nc.(*conn))
		MarkOrdered(nc)
		if _, err := nc.Write(make([]byte, 517)); err != nil {
			t.Error(err)
		}
		if err := nc.Close(); err != nil {
			t.Error(err)
		}
	}
	// whileBulk runs the poll and a handshake, twice, while the transfer has
	// a request crossing every bulk lane.
	whileBulk := func(transfer func() error) {
		done := make(chan error, 1)
		go func() { done <- transfer() }()
		for round := 0; round < 2; round++ {
			deadline := time.Now().Add(5 * time.Second)
			for {
				busy := 0
				for _, l := range links {
					if l.up.queued.Load()+l.down.queued.Load() > 1<<10 {
						busy++
					}
				}
				if busy >= DownWindowSize || time.Now().After(deadline) {
					break
				}
				time.Sleep(time.Millisecond)
			}
			poll()
			handshake()
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}

	nc, err := Dial(t.Context(), srv.URL, "s", "agent", "tok")
	if err != nil {
		t.Fatal(err)
	}
	c := nc.(*conn)
	clients(c)
	MarkOrdered(nc)
	MarkWindow(nc)
	// The device serves a pull...
	whileBulk(func() error {
		if _, err := nc.Write(payload); err != nil {
			return err
		}
		return c.flushWrites()
	})
	// ...and takes a push.
	whileBulk(func() error {
		_, err := io.ReadFull(nc, make([]byte, len(payload)))
		return err
	})
	if err := nc.Close(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if uploaded < len(payload) {
		t.Errorf("uploaded %d bytes of %d", uploaded, len(payload))
	}
	mu.Unlock()

	for i, l := range links {
		l.mu.Lock()
		fullUps, wants, ahead := l.fullUps, l.wants, l.queuedAhead
		l.mu.Unlock()
		if i == 0 {
			if fullUps != 0 || wants != 0 {
				t.Errorf("lane 0 carried %d full upload batches and %d windowed downloads", fullUps, wants)
			}
			for _, n := range ahead {
				if n > 1<<10 {
					t.Errorf("a poll or handshake on lane 0 queued behind %d bytes: %v", n, ahead)
					break
				}
			}
			continue
		}
		if fullUps == 0 || wants == 0 {
			t.Errorf("lane %d carried %d full upload batches and %d windowed downloads", i, fullUps, wants)
		}
	}
}
