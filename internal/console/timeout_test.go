package console

import (
	"testing"
	"time"

	"wanctl/internal/policy"
)

func TestSetTimeoutClampsAndRestores(t *testing.T) {
	s := newSvc(t)

	if got := s.Timeout(); got != DefaultTimeout {
		t.Fatalf("initial timeout: got %s, want %s", got, DefaultTimeout)
	}
	for _, tc := range []struct {
		name string
		set  time.Duration
		want time.Duration
	}{
		{"a phone-sized wait is honoured", 3 * time.Minute, 3 * time.Minute},
		{"zero restores the default", 0, DefaultTimeout},
		{"below the floor is clamped up", time.Second, MinTimeout},
		{"above the ceiling is clamped down", time.Hour, MaxTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if applied := s.SetTimeout(tc.set); applied != tc.want {
				t.Fatalf("SetTimeout(%s) applied %s, want %s", tc.set, applied, tc.want)
			}
			if got := s.Timeout(); got != tc.want {
				t.Fatalf("Timeout() after SetTimeout(%s): got %s, want %s", tc.set, got, tc.want)
			}
		})
	}
}

// TestAskHonoursTimeoutChangedAfterConstruction is the point of the whole
// feature: a raised wait must apply to approvals asked afterwards, and Ask must
// read the current value rather than one captured when the service was built.
func TestAskHonoursTimeoutChangedAfterConstruction(t *testing.T) {
	s := newSvc(t)
	// Subscribe so Ask blocks for a decision instead of denying immediately.
	_, unsub := s.Subscribe()
	defer unsub()

	s.SetTimeout(MinTimeout) // clamped floor, still far longer than this test waits

	done := make(chan policy.Decision, 1)
	go func() {
		done <- s.Ask(policy.Request{Kind: policy.KindExec, Cmd: "true"})
	}()

	// Give Ask time to enqueue, then decide through the queue. If Ask had used a
	// stale sub-second timeout it would already have denied.
	var id string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p := s.State().Pending; len(p) > 0 {
			id = p[0].ID
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("approval never appeared in the pending set")
	}
	if !s.Decide(id, "y") {
		t.Fatal("Decide reported no such pending approval")
	}

	select {
	case d := <-done:
		if !d.Allow {
			t.Fatal("approval was denied despite an in-window allow decision")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ask did not return after being decided")
	}
}
