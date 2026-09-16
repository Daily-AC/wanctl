package webfetch

import (
	"fmt"
	"testing"
	"time"
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
