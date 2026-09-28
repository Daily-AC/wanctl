package console

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// countHook counts pairing notifications, which on a live device are reports
// that end up on the owner's phone.
func countHook(s *Service) *atomic.Int64 {
	var n atomic.Int64
	s.SetPairingHook(func(PendingPairing) { n.Add(1) })
	return &n
}

// Anyone who can get a session routed to the device can present a fresh key on
// every dial. Neither the device's memory nor the owner's notifications may
// grow with the number of keys: past a small number of waiting requests, new
// fingerprints are refused outright.
func TestPendingPairingsAreBounded(t *testing.T) {
	s := newSvc(t)
	hooks := countHook(s)
	var refused int
	for i := range 5000 {
		if _, err := s.AskPairNonBlocking(fmt.Sprintf("SHA256:flood-%d", i), "flood", "please trust me"); errors.Is(err, ErrTooManyPairings) {
			refused++
		}
	}
	if got := len(s.State().PendingPairings); got > maxPendingPairs {
		t.Fatalf("%d pairing requests retained, want at most %d", got, maxPendingPairs)
	}
	s.mu.Lock()
	held, noted := len(s.pairs), len(s.notified)
	s.mu.Unlock()
	if held > maxPendingPairs || noted > maxPendingPairs {
		t.Fatalf("memory grew with the flood: %d entries, %d notification records", held, noted)
	}
	if refused != 5000-maxPendingPairs {
		t.Fatalf("%d requests refused, want every one past the first %d", refused, maxPendingPairs)
	}
	// The hook runs on its own goroutine; give stragglers a moment to land.
	time.Sleep(50 * time.Millisecond)
	if got := hooks.Load(); got > maxPendingPairs {
		t.Fatalf("the owner was notified %d times, want at most %d", got, maxPendingPairs)
	}

	// A request that was already waiting is not pushed out by the flood: the
	// owner can still approve it, and its controller then gets in.
	if !s.DecidePair("SHA256:flood-0", true) {
		t.Fatal("a request queued before the flood can no longer be approved")
	}
	if ok, err := s.AskPairNonBlocking("SHA256:flood-0", "flood", ""); !ok || err != nil {
		t.Fatalf("approved controller: %v %v", ok, err)
	}
}

// Asking again with the same key does not ask the owner again. That holds even
// after the owner said no, which removes the request: a controller that keeps
// retrying gets a card in the portal, not a message on the owner's phone each
// time.
func TestPairingNotifiesOncePerFingerprint(t *testing.T) {
	s := newSvc(t)
	hooks := countHook(s)
	for range 3 {
		s.AskPairNonBlocking("SHA256:retry", "laptop", "someone")
		s.DecidePair("SHA256:retry", false)
	}
	s.AskPairNonBlocking("SHA256:other", "laptop", "someone else")
	time.Sleep(50 * time.Millisecond)
	if got := hooks.Load(); got != 2 {
		t.Fatalf("notifications = %d, want one for each of the two fingerprints", got)
	}
	if len(s.State().PendingPairings) != 1 {
		t.Fatalf("the portal must still show the waiting request: %+v", s.State().PendingPairings)
	}
}

// The owner's approval is for the dial that is waiting on it. It is spent when
// that controller collects it, and it lapses if the controller never comes back
// for it; either way a later dial with that key is a new question.
func TestPairingApprovalIsCollectedOnce(t *testing.T) {
	s := newSvc(t)
	s.AskPairNonBlocking("SHA256:once", "laptop", "someone")
	s.DecidePair("SHA256:once", true)
	if ok, _ := s.AskPairNonBlocking("SHA256:once", "laptop", "someone"); !ok {
		t.Fatal("the approved controller was not let in")
	}
	if ok, _ := s.AskPairNonBlocking("SHA256:once", "laptop", "someone"); ok {
		t.Fatal("one approval admitted the same key twice")
	}

	s.AskPairNonBlocking("SHA256:lapsed", "laptop", "someone")
	s.DecidePair("SHA256:lapsed", true)
	s.mu.Lock()
	s.pairs["SHA256:lapsed"].expires = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if ok, _ := s.AskPairNonBlocking("SHA256:lapsed", "laptop", "someone"); ok {
		t.Fatal("an approval nobody collected was still honoured after it expired")
	}
}
