package webfetch

import (
	"fmt"
	"testing"
	"time"

	"wanctl/internal/delegation"
)

func TestRateWindowCapacityRecoversAfterExpiry(t *testing.T) {
	h := &Handler{requests: make(map[string]rateWindow)}
	for i := 0; i < 4096; i++ {
		if !h.rateAllowed(fmt.Sprintf("grant:%d", i), 120) {
			t.Fatalf("window %d denied before capacity", i)
		}
	}
	if h.rateAllowed("new:live-overflow", 10) {
		t.Fatal("new window exceeded the live window cap")
	}
	if len(h.requests) != 4096 {
		t.Fatalf("window count = %d, want 4096", len(h.requests))
	}
	// Request IDs are attacker controlled. Filling the map must only consume
	// this time window, never permanently prevent new sessions until restart.
	for key, window := range h.requests {
		window.at = time.Now().Add(-2 * time.Minute)
		h.requests[key] = window
	}
	if !h.rateAllowed("new:after-expiry", 10) {
		t.Fatal("expired windows permanently exhausted rate-limiter capacity")
	}
	if len(h.requests) != 1 {
		t.Fatalf("window count after pruning = %d, want 1", len(h.requests))
	}
	for i := 1; i < 10; i++ {
		if !h.rateAllowed("new:after-expiry", 10) {
			t.Fatalf("request %d unexpectedly denied", i+1)
		}
	}
	if h.rateAllowed("new:after-expiry", 10) {
		t.Fatal("per-window rate limit lost after capacity recovery")
	}
}

// The browser ticket's envelope is the session URL's lifetime, so it has to
// cover the longest grant the ticket can carry plus the window that grant has
// to be approved in. The boundary is asserted with an injected clock rather
// than by waiting for it.
func TestTicketEnvelopeCoversTheLongestGrant(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		issue time.Duration // relative to now; negative is in the past
		fresh bool
	}{
		{"just issued", 0, true},
		{"inside the old seventy-minute envelope", -65 * time.Minute, true},
		{"past the old envelope but inside a day-long grant", -1445 * time.Minute, true},
		{"one minute inside the envelope", -(delegation.TicketLifetime - time.Minute), true},
		{"exactly at the envelope", -delegation.TicketLifetime, false},
		{"past the envelope", -1451 * time.Minute, false},
		{"small clock skew from the future", 20 * time.Second, true},
		{"a forged future timestamp", 2 * time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ticketFresh(now.Add(tc.issue), now); got != tc.fresh {
				t.Fatalf("ticketFresh(now%+v) = %v, want %v", tc.issue, got, tc.fresh)
			}
		})
	}
	if delegation.TicketLifetime != 1450*time.Minute {
		t.Fatalf("envelope = %v, want the 1440-minute ceiling plus the 10-minute request window", delegation.TicketLifetime)
	}
}
