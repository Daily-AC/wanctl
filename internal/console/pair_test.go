package console

import (
	"testing"
	"time"
)

func TestAskPairBlocksUntilDecide(t *testing.T) {
	s := newSvc(t)
	s.timeout = time.Second
	_, cancel := s.Subscribe()
	defer cancel()

	res := make(chan bool, 1)
	go func() { res <- s.AskPair("SHA256:abc", "thunder-2", "巡检 home-pc") }()

	// the pending pairing shows up in State
	var fp, label string
	for i := 0; i < 50; i++ {
		if p := s.State().PendingPairings; len(p) == 1 {
			fp = p[0].FP
			label = p[0].Label
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fp != "SHA256:abc" {
		t.Fatalf("pending pairing never appeared (fp=%q)", fp)
	}
	if label != "巡检 home-pc" {
		t.Fatalf("pairing label not propagated: %q", label)
	}
	if !s.DecidePair(fp, true) {
		t.Fatal("DecidePair missed the pending entry")
	}
	if !<-res {
		t.Fatal("AskPair should have returned true after trust")
	}
	// resolved pairing clears from state
	if len(s.State().PendingPairings) != 0 {
		t.Fatal("pairing not cleared after decision")
	}
}

func TestAskPairDeniesWithoutFrontend(t *testing.T) {
	s := newSvc(t)
	s.timeout = time.Second
	// no Subscribe() -> no front-end -> must deny immediately, not block.
	start := time.Now()
	if s.AskPair("SHA256:x", "nobody", "") {
		t.Fatal("AskPair must deny when no front-end is connected")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("AskPair blocked instead of denying immediately")
	}
}

func TestDecidePairDenyReturnsFalse(t *testing.T) {
	s := newSvc(t)
	s.timeout = time.Second
	_, cancel := s.Subscribe()
	defer cancel()
	res := make(chan bool, 1)
	go func() { res <- s.AskPair("SHA256:deny", "evil", "") }()
	for i := 0; i < 50 && len(s.State().PendingPairings) == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	s.DecidePair("SHA256:deny", false)
	if <-res {
		t.Fatal("AskPair should return false when denied")
	}
}
