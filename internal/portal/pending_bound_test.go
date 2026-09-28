package portal

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Polls of /api/pending that arrive while one aggregate is running for the
// same namespace wait for its answer instead of starting their own.
func TestConcurrentPendingPollsShareOneAggregate(t *testing.T) {
	var listed atomic.Int32
	release := make(chan struct{})
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "bob"})
		case "/admin/devices":
			listed.Add(1)
			<-release
			json.NewEncoder(w).Encode(map[string]any{"devices": []any{}})
		default:
			http.NotFound(w, r)
		}
	})
	const polls = 20
	var wg sync.WaitGroup
	codes := make(chan int, polls)
	for i := 0; i < polls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/api/pending", nil)
			req.Header.Set("X-User", "bob@example.com")
			rec := httptest.NewRecorder()
			s.handleWaiting(rec, req)
			codes <- rec.Code
		}()
	}
	// Let every poll reach the aggregate before it finishes.
	deadline := time.Now().Add(2 * time.Second)
	for listed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusOK {
			t.Fatalf("a poll got %d", c)
		}
	}
	if n := listed.Load(); n != 1 {
		t.Fatalf("%d concurrent polls listed devices %d times; want 1", polls, n)
	}
}

// The aggregate skips a device that is busy answering another request rather
// than queueing a goroutine behind it.
func TestStateIfIdleSkipsABusyDevice(t *testing.T) {
	cli, srv := net.Pipe()
	fakeDevice(t, srv)
	d := newDeviceConn(cli)
	defer d.close()

	d.rpcMu.Lock()
	start := time.Now()
	_, err := d.stateIfIdle(time.Second)
	d.rpcMu.Unlock()
	if !errors.Is(err, errDeviceBusy) {
		t.Fatalf("err = %v, want errDeviceBusy", err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatalf("waited %s for a busy device", time.Since(start))
	}
	if _, err := d.stateIfIdle(time.Second); err != nil {
		t.Fatalf("idle device: %v", err)
	}
}
