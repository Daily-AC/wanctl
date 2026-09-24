package relay

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func drainAll(t *testing.T, q *sideQueue) []byte {
	t.Helper()
	data, _ := q.drain(context.Background(), 50*time.Millisecond)
	return data
}

func TestSequencedWritesAreQueuedInOrder(t *testing.T) {
	q := newSideQueue()
	for _, w := range []struct {
		seq  uint64
		body string
	}{{3, "c"}, {1, "a"}, {4, "d"}, {2, "b"}} {
		if !q.pushSeq(w.seq, []byte(w.body)) {
			t.Fatalf("write %d refused", w.seq)
		}
	}
	if got := drainAll(t, q); !bytes.Equal(got, []byte("abcd")) {
		t.Fatalf("queued %q, want %q", got, "abcd")
	}
}

func TestRepeatedWriteIsNotQueuedTwice(t *testing.T) {
	q := newSideQueue()
	q.pushSeq(1, []byte("a"))
	q.pushSeq(2, []byte("b"))
	if !q.pushSeq(1, []byte("a")) {
		t.Fatal("a retry of a delivered write must be acknowledged")
	}
	q.pushSeq(3, []byte("c"))
	if got := drainAll(t, q); !bytes.Equal(got, []byte("abc")) {
		t.Fatalf("queued %q, want %q", got, "abc")
	}
}

func TestEmptySequencedWriteStillTakesItsPlace(t *testing.T) {
	q := newSideQueue()
	q.pushSeq(2, []byte("b"))
	q.pushSeq(1, nil)
	if got := drainAll(t, q); !bytes.Equal(got, []byte("b")) {
		t.Fatalf("queued %q, want %q", got, "b")
	}
}

func TestWriteFarAheadIsRefused(t *testing.T) {
	q := newSideQueue()
	if q.pushSeq(2+maxSeqAhead, []byte("x")) {
		t.Fatal("a write beyond the window must be refused, or one session could make the relay hold unbounded data")
	}
}
