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
}

func (l *failingLane) RoundTrip(req *http.Request) (*http.Response, error) {
	l.mu.Lock()
	if req.URL.Path == "/h/up" {
		l.ups++
	}
	if req.URL.Path == "/h/down" {
		l.downs++
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

func TestLaneFailuresReplay32MiB(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789abcdef"), 2<<20)
	var mu sync.Mutex
	uploads := map[uint64][]byte{}
	var downMu sync.Mutex
	downAssigned := map[uint64][]byte{}
	downNext := uint64(1)
	downOffset := 0
	downChanged := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/h/up":
			seq, _ := strconv.ParseUint(r.URL.Query().Get(UpSeqParam), 10, 64)
			body, _ := io.ReadAll(r.Body)
			time.Sleep(10 * time.Millisecond) // keep four uploads in flight
			mu.Lock()
			uploads[seq] = body
			mu.Unlock()
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
				downMu.Lock()
				if assigned, ok := downAssigned[want]; ok {
					chunk = assigned
					downMu.Unlock()
					break
				}
				if want == downNext {
					if downOffset < len(payload) {
						end := min(downOffset+max, len(payload))
						chunk = payload[downOffset:end]
						downAssigned[want] = chunk
						downOffset = end
						downNext++
						close(downChanged)
						downChanged = make(chan struct{})
					}
					downMu.Unlock()
					break
				}
				changed := downChanged
				downMu.Unlock()
				select {
				case <-changed:
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
	defer srv.Close()
	nc, err := Dial(t.Context(), srv.URL, "s", "client", "tok")
	if err != nil {
		t.Fatal(err)
	}
	c := nc.(*conn)
	var lanes [4]*failingLane
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
		ups, downs, drops, cuts, closed := l.ups, l.downs, l.drops, l.cuts, l.closed
		l.mu.Unlock()
		if ups == 0 || downs == 0 {
			t.Errorf("lane %d requests: up=%d down=%d", i, ups, downs)
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

func TestBulkUploadUsesFourLanes(t *testing.T) {
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	backend := &pipelineTransport{started: started, release: release}
	var counters [4]*countingTransport
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
		if up == 0 {
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
	var counters [4]*countingTransport
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
	var counters [4]*countingTransport
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
