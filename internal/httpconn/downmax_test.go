package httpconn

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

// A reader asks for bigger chunks while full ones keep arriving quickly, up to
// the ceiling, and backs off when one arrives slowly.
func TestDownPollChunkGrowsAndShrinks(t *testing.T) {
	var (
		mu    sync.Mutex
		asked []int
		seq   uint64
		slow  bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/h/down" {
			w.WriteHeader(http.StatusOK)
			return
		}
		n, _ := strconv.Atoi(req.URL.Query().Get(DownMaxParam))
		mu.Lock()
		asked = append(asked, n)
		seq++
		s, delay := seq, slow
		mu.Unlock()
		w.Header().Set(DownAckCapabilityHeader, "1")
		w.Header().Set(DownSeqHeader, strconv.FormatUint(s, 10))
		w.Header().Set("Content-Length", strconv.Itoa(n))
		w.WriteHeader(http.StatusOK)
		if delay {
			// Headers first, then the body late: the reader times the body.
			w.(http.Flusher).Flush()
			time.Sleep(downShrinkPast + 100*time.Millisecond)
		}
		w.Write(make([]byte, n))
	}))
	defer srv.Close()
	c, err := Dial(context.Background(), srv.URL, "s", "agent", "tok")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	readChunk := func() {
		t.Helper()
		// One poll's worth: read until the reader has drained its leftover.
		cc := c.(*conn)
		buf := make([]byte, 1<<20)
		for {
			if _, err := c.Read(buf); err != nil {
				t.Fatal(err)
			}
			if len(cc.leftover) == 0 {
				return
			}
		}
	}
	for range 5 {
		readChunk()
	}
	want := []int{2 << 20, 4 << 20, 8 << 20, 16 << 20, 16 << 20}
	mu.Lock()
	got := append([]int(nil), asked...)
	mu.Unlock()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chunk sizes asked = %v, want %v", got, want)
		}
	}

	old := downShrinkPast
	downShrinkPast = 200 * time.Millisecond
	t.Cleanup(func() { downShrinkPast = old })
	mu.Lock()
	slow = true
	mu.Unlock()
	readChunk() // takes longer than downShrinkPast
	mu.Lock()
	slow = false
	mu.Unlock()
	readChunk()
	mu.Lock()
	last := asked[len(asked)-1]
	mu.Unlock()
	if last != 8<<20 {
		t.Fatalf("after a slow chunk the reader asked for %d, want it halved to %d", last, 8<<20)
	}
}
