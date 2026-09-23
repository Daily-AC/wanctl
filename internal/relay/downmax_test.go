package relay

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"wanctl/internal/httpconn"
)

// A poll carries as much as its reader asks for, up to maxDrainLimit, and
// maxDrainBytes when it does not say. The length is declared: the relay's CDN
// path slows a long response of unknown length to a crawl.
func TestDownPollHonoursReaderChunkSize(t *testing.T) {
	cases := map[string]struct {
		max  string
		want int
	}{
		"unset":         {"", maxDrainBytes},
		"eight MiB":     {strconv.Itoa(8 << 20), 8 << 20},
		"over the cap":  {strconv.Itoa(64 << 20), maxDrainLimit},
		"not a number":  {"lots", maxDrainBytes},
		"smaller chunk": {strconv.Itoa(1 << 20), 1 << 20},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r, s, sid := tunnelSession(t)
			queued := bytes.Repeat([]byte("x"), 1<<20)
			for range (maxDrainLimit + 4<<20) / len(queued) {
				s.toAgent.push(queued)
			}
			url := "/h/down?session=" + sid + "&role=agent&ack=0"
			if tc.max != "" {
				url += "&" + httpconn.DownMaxParam + "=" + tc.max
			}
			req := httptest.NewRequest("GET", url, nil)
			req.Header.Set("Authorization", "Bearer tok-alice")
			rec := httptest.NewRecorder()
			r.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || rec.Body.Len() != tc.want {
				t.Fatalf("poll = %d with %d bytes, want 200 with %d", rec.Code, rec.Body.Len(), tc.want)
			}
			if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(tc.want) {
				t.Fatalf("Content-Length = %q, want %d", got, tc.want)
			}
		})
	}
}

// A session holding a chunk its reader has not acknowledged outlives the
// ordinary idle limit: the reader may still be downloading it from a proxy's
// buffer on a slow link, and cannot poll until it has it.
func TestSessionHoldingUnackedChunkOutlivesIdleLimit(t *testing.T) {
	r, s, sid := tunnelSession(t)
	s.toAgent.push([]byte("HELD"))
	req := httptest.NewRequest("GET", "/h/down?session="+sid+"&role=agent&ack=0", nil)
	req.Header.Set("Authorization", "Bearer tok-alice")
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("poll = %d", rec.Code)
	}

	r.reapHTTP(time.Now().Add(httpSessionIdle + time.Minute))
	if r.session(sid) == nil {
		t.Fatal("a session holding an unacknowledged chunk was reaped at the ordinary idle limit")
	}
	r.reapHTTP(time.Now().Add(unackedRetention + time.Minute))
	if r.session(sid) != nil {
		t.Fatal("a session nobody polled for longer than the retention was kept")
	}
}
