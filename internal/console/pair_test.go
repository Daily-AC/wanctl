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

// URL-flow: AskPair without a subscriber returns false fast but LEAVES the
// pending entry so the user can click the link the AI surfaced and approve
// retroactively. The next AskPair (controller retry) then returns true.
func TestAskPairPersistsForRetroactiveApproval(t *testing.T) {
	s := newSvc(t)
	s.timeout = time.Second

	// No subscriber: should fail fast …
	if s.AskPair("SHA256:late", "thunder-2", "Claude X") {
		t.Fatal("expected false with no front-end")
	}
	// … but the entry persists for the URL-click flow.
	if len(s.State().PendingPairings) != 1 {
		t.Fatalf("pair entry not persisted: %+v", s.State().PendingPairings)
	}
	// Retroactive approval (the SPA POSTing /api/devices/pair).
	if !s.DecidePair("SHA256:late", true) {
		t.Fatal("DecidePair on persisted entry should succeed")
	}
	// Same controller retries → now trusted.
	if !s.AskPair("SHA256:late", "thunder-2", "Claude X") {
		t.Fatal("AskPair after retroactive approval should return true")
	}
	// And no longer shown in the UI.
	if len(s.State().PendingPairings) != 0 {
		t.Fatalf("approved pairing should not show as pending: %+v", s.State().PendingPairings)
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

// An undecided pairing past pairTTL must stop being offered to the portal, and
// must stop being approvable. Only askPair pruned, so a request nobody dialled
// again stayed on screen forever; the portal kept drawing a card whose fp the
// device would no longer honour, and clicking it looked like a dead button
// (issue #79).
func TestExpiredPairingLeavesTheConsole(t *testing.T) {
	s := newSvc(t)
	s.timeout = time.Second

	// URL flow: no front-end attending, entry persists for a retroactive click.
	if s.AskPair("SHA256:stale", "kestrel", "claude-code on kestrel") {
		t.Fatal("expected false with no front-end")
	}
	if len(s.State().PendingPairings) != 1 {
		t.Fatalf("pair entry not persisted: %+v", s.State().PendingPairings)
	}

	// Five minutes later.
	s.mu.Lock()
	s.pairs["SHA256:stale"].expires = time.Now().Add(-time.Second)
	s.mu.Unlock()

	if got := s.State().PendingPairings; len(got) != 0 {
		t.Fatalf("expired pairing still offered to the portal: %+v", got)
	}
	if s.DecidePair("SHA256:stale", true) {
		t.Fatal("an expired pairing request must not still be approvable")
	}
}

// Same guarantee reached from the other side: the verdict arrives first, with
// nothing having called State(). DecidePair must not trust an expired request.
func TestDecidePairRefusesExpiredWithoutAStateRead(t *testing.T) {
	s := newSvc(t)
	s.timeout = time.Second
	s.AskPair("SHA256:late", "kestrel", "")
	s.mu.Lock()
	s.pairs["SHA256:late"].expires = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if s.DecidePair("SHA256:late", true) {
		t.Fatal("DecidePair trusted a request that had already expired")
	}
	if s.AskPair("SHA256:late", "kestrel", "") {
		t.Fatal("controller retry must not find itself trusted")
	}
}
