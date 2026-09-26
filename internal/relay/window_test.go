package relay

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

func windowPoll(t *testing.T, r *Relay, sid string, ack, want uint64) *httptest.ResponseRecorder {
	t.Helper()
	u := "/h/down?session=" + sid + "&role=agent&ack=" + strconv.FormatUint(ack, 10) + "&want=" + strconv.FormatUint(want, 10) + "&max=1"
	req := httptest.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("Authorization", "Bearer tok-alice")
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	return rec
}

func TestWindowPollBoundsAndReplay(t *testing.T) {
	r, s, sid := tunnelSession(t)
	for _, want := range []uint64{0, 5} {
		if got := windowPoll(t, r, sid, 0, want).Code; got != http.StatusBadRequest {
			t.Fatalf("want=%d: status=%d, want 400", want, got)
		}
	}
	for range 5 {
		s.toAgent.push([]byte("x"))
	}
	for k := uint64(1); k <= 4; k++ {
		resp := windowPoll(t, r, sid, 0, k)
		if resp.Code != http.StatusOK || resp.Body.String() != "x" || resp.Header().Get("X-Wanctl-Down-Seq") != strconv.FormatUint(k, 10) {
			t.Fatalf("want=%d: status=%d body=%q seq=%q", k, resp.Code, resp.Body.String(), resp.Header().Get("X-Wanctl-Down-Seq"))
		}
		if resp.Header().Get("X-Wanctl-Down-Window") != "4" {
			t.Fatal("missing window advertisement")
		}
	}
	s.toAgent.ackMu.Lock()
	held := len(s.toAgent.assigned)
	s.toAgent.ackMu.Unlock()
	if held != 4 {
		t.Fatalf("held %d chunks, want 4", held)
	}
	first := windowPoll(t, r, sid, 0, 1)
	if first.Code != 200 || first.Body.String() != "x" || first.Header().Get("X-Wanctl-Down-Seq") != "1" {
		t.Fatalf("replay status=%d body=%q seq=%q", first.Code, first.Body.String(), first.Header().Get("X-Wanctl-Down-Seq"))
	}
	if got := windowPoll(t, r, sid, 1, 1).Code; got != http.StatusBadRequest {
		t.Fatalf("acknowledged want: status=%d, want 400", got)
	}
	if got := windowPoll(t, r, sid, 1, 5).Code; got != http.StatusOK {
		t.Fatalf("moving window: status=%d", got)
	}
}

func TestConcurrentWindowPollsAssignInOrder(t *testing.T) {
	q := newSideQueue()
	for i := byte(1); i <= 4; i++ {
		q.push([]byte{i})
	}
	var wg sync.WaitGroup
	for k := uint64(1); k <= 4; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data, seq, closed, ok := q.takeWindow(context.Background(), 0, k, time.Second, 1)
			if !ok || closed || seq != k || !bytes.Equal(data, []byte{byte(k)}) {
				t.Errorf("want=%d: data=%v seq=%d closed=%v ok=%v", k, data, seq, closed, ok)
			}
		}()
	}
	wg.Wait()
}

func TestWindowAssignedChunkPreventsSettlement(t *testing.T) {
	q := newSideQueue()
	q.push([]byte("held"))
	q.close()
	if _, _, _, ok := q.takeWindow(context.Background(), 0, 1, time.Second, 4); !ok {
		t.Fatal("poll was not served")
	}
	if q.settled() {
		t.Fatal("queue settled with an assigned chunk")
	}
	q.takeWindow(context.Background(), 1, 2, time.Millisecond, 4)
	if !q.settled() {
		t.Fatal("queue did not settle after acknowledgement")
	}
}
