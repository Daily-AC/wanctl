package httpconn

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type uploadRecorder struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (r *uploadRecorder) handler(w http.ResponseWriter, req *http.Request) {
	switch req.URL.Path {
	case "/h/up":
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.bodies = append(r.bodies, body)
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case "/h/down":
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("x"))
	case "/h/close":
		w.WriteHeader(http.StatusOK)
	default:
		http.NotFound(w, req)
	}
}

func (r *uploadRecorder) snapshot() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]byte, len(r.bodies))
	copy(out, r.bodies)
	return out
}

func TestWritesAreBatchedBeforeRead(t *testing.T) {
	recorder := &uploadRecorder{}
	server := httptest.NewServer(http.HandlerFunc(recorder.handler))
	defer server.Close()
	connection, err := Dial(t.Context(), server.URL, "session", "client", "token")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()

	chunk := bytes.Repeat([]byte("a"), 4096)
	for range 16 {
		if _, err := connection.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	buf := make([]byte, 1)
	if _, err := connection.Read(buf); err != nil {
		t.Fatal(err)
	}
	bodies := recorder.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("upload requests = %d, want 1", len(bodies))
	}
	if len(bodies[0]) != 16*len(chunk) {
		t.Fatalf("uploaded bytes = %d, want %d", len(bodies[0]), 16*len(chunk))
	}
}

func TestWriteBatchFlushesAtBound(t *testing.T) {
	recorder := &uploadRecorder{}
	server := httptest.NewServer(http.HandlerFunc(recorder.handler))
	defer server.Close()
	connection, err := Dial(t.Context(), server.URL, "session", "client", "token")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()

	if _, err := connection.Write(make([]byte, writeBatchBytes)); err != nil {
		t.Fatal(err)
	}
	bodies := recorder.snapshot()
	if len(bodies) != 1 || len(bodies[0]) != writeBatchBytes {
		t.Fatalf("batches = %v, want one %d-byte request", bodyLengths(bodies), writeBatchBytes)
	}
}

func TestSmallWriteFlushesOnTimer(t *testing.T) {
	recorder := &uploadRecorder{}
	server := httptest.NewServer(http.HandlerFunc(recorder.handler))
	defer server.Close()
	conn, err := Dial(t.Context(), server.URL, "session", "client", "token")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("small")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(recorder.snapshot()) == 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("small write was not flushed by the timer")
}

func TestCloseFlushesPendingWrite(t *testing.T) {
	recorder := &uploadRecorder{}
	server := httptest.NewServer(http.HandlerFunc(recorder.handler))
	defer server.Close()
	conn, err := Dial(t.Context(), server.URL, "session", "client", "token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("tail")); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	bodies := recorder.snapshot()
	if len(bodies) != 1 || string(bodies[0]) != "tail" {
		t.Fatalf("close-flushed bodies = %q, want [tail]", bodies)
	}
}

func TestLargeWriteIsSplitAtBatchBound(t *testing.T) {
	recorder := &uploadRecorder{}
	server := httptest.NewServer(http.HandlerFunc(recorder.handler))
	defer server.Close()
	connection, err := Dial(t.Context(), server.URL, "session", "client", "token")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()

	payload := make([]byte, 2*writeBatchBytes+1)
	if _, err := connection.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := connection.(*conn).flushWrites(); err != nil {
		t.Fatal(err)
	}
	lengths := bodyLengths(recorder.snapshot())
	want := []int{writeBatchBytes, writeBatchBytes, 1}
	if len(lengths) != len(want) {
		t.Fatalf("batch lengths = %v, want %v", lengths, want)
	}
	for i := range want {
		if lengths[i] != want[i] {
			t.Fatalf("batch lengths = %v, want %v", lengths, want)
		}
	}
}

func bodyLengths(bodies [][]byte) []int {
	lengths := make([]int, len(bodies))
	for i := range bodies {
		lengths[i] = len(bodies[i])
	}
	return lengths
}

// TestBulkWriteSendsNoTailRequests models a push: the TLS layer hands over one
// record at a time, records do not divide the batch size, and each upload takes
// longer than the flush delay. The timer armed before a batch filled must not
// survive the batch and post its leftover as an extra request.
func TestBulkWriteSendsNoTailRequests(t *testing.T) {
	recorder := &uploadRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/h/up" {
			time.Sleep(50 * time.Millisecond)
		}
		recorder.handler(w, req)
	}))
	defer server.Close()
	connection, err := Dial(t.Context(), server.URL, "session", "client", "token")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()

	const record = 16413 // a full TLS 1.3 record with its overhead
	total := 0
	for total < 3*writeBatchBytes {
		if _, err := connection.Write(make([]byte, record)); err != nil {
			t.Fatal(err)
		}
		total += record
	}
	if err := connection.(*conn).flushWrites(); err != nil {
		t.Fatal(err)
	}
	lengths := bodyLengths(recorder.snapshot())
	want := (total + writeBatchBytes - 1) / writeBatchBytes
	if len(lengths) != want {
		t.Fatalf("%d bytes went out in %d requests %v, want %d", total, len(lengths), lengths, want)
	}
}
