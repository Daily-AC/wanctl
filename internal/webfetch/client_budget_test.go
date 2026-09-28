package webfetch

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"wanctl/internal/delegation"
)

// creatingStore answers every fresh ticket with a pending request, which is
// what lets a test reach the per-client budget that guards creating one.
type creatingStore struct{ emptyStore }

func (creatingStore) CreateDelegation(_ context.Context, n delegation.NewRequest) (delegation.Request, error) {
	return delegation.Request{ID: n.ID, Status: "pending", RequestExpiresAt: n.RequestExpiresAt}, nil
}

func budgetHandler(t *testing.T) *Handler {
	t.Helper()
	h, err := New(Config{
		Store: creatingStore{}, Jobs: creatingStore{}, Seed: bytes.Repeat([]byte{7}, 32),
		RelayURL: "https://relay.example", PublicOrigin: "https://relay.example", PortalOrigin: "https://portal.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

// newAccessRequest asks for a new delegation the way a web AI's fetcher does,
// arriving from remoteAddr with the given headers, and returns the status. It
// asks for JSON: the HTML page answers a refusal with 200 so that extractors
// which drop error bodies still show it, and the real status is what counts.
func newAccessRequest(t *testing.T, h *Handler, remoteAddr string, headers map[string]string) int {
	t.Helper()
	nonce := make([]byte, 24)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/webfetch/new/"+hex.EncodeToString(nonce)+"?format=json", nil)
	req.RemoteAddr = remoteAddr
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code
}

// Every request reaches the relay from the reverse proxy in front of it, so a
// budget keyed on the TCP peer is one budget for the whole internet. The proxy
// names the client in X-Real-IP; spending one client's budget must leave
// another client's alone.
func TestNewAccessRequestBudgetIsPerClientBehindTheProxy(t *testing.T) {
	h := budgetHandler(t)
	const proxy = "127.0.0.1:51000"
	first := map[string]string{"X-Real-IP": "198.51.100.1"}
	for i := 0; i < 10; i++ {
		if code := newAccessRequest(t, h, proxy, first); code != http.StatusOK {
			t.Fatalf("request %d from one client = %d, want 200", i+1, code)
		}
	}
	if code := newAccessRequest(t, h, proxy, first); code != http.StatusTooManyRequests {
		t.Fatalf("11th request from one client = %d, want 429", code)
	}
	if code := newAccessRequest(t, h, proxy, map[string]string{"X-Real-IP": "198.51.100.2"}); code != http.StatusOK {
		t.Fatalf("another client behind the same proxy = %d, want 200: the budget is shared by everyone", code)
	}
	// A proxy reaching a container through a Docker port mapping arrives from
	// the bridge gateway, a private address, rather than from loopback.
	if code := newAccessRequest(t, h, "172.18.0.1:40000", map[string]string{"X-Real-IP": "198.51.100.3"}); code != http.StatusOK {
		t.Fatalf("a client behind a bridge-network proxy = %d, want 200", code)
	}
}

// Only the proxy's own header is believed, and only from the proxy. A client
// that reaches the relay directly cannot rename itself, and X-Forwarded-For —
// which a proxy appends to whatever the client sent — never names a bucket.
func TestForgedClientAddressHeadersDoNotSplitTheBudget(t *testing.T) {
	h := budgetHandler(t)
	const direct = "203.0.113.9:40000"
	for i := 0; i < 10; i++ {
		forged := map[string]string{
			"X-Real-IP":       fmt.Sprintf("198.51.100.%d", i+1),
			"X-Forwarded-For": fmt.Sprintf("192.0.2.%d", i+1),
		}
		if code := newAccessRequest(t, h, direct, forged); code != http.StatusOK {
			t.Fatalf("direct request %d = %d, want 200", i+1, code)
		}
	}
	if code := newAccessRequest(t, h, direct, map[string]string{"X-Real-IP": "198.51.100.200"}); code != http.StatusTooManyRequests {
		t.Fatalf("a direct client renamed itself with X-Real-IP and got %d, want 429", code)
	}

	const proxy = "127.0.0.1:51000"
	for i := 0; i < 10; i++ {
		spoofed := map[string]string{"X-Real-IP": "198.51.100.50", "X-Forwarded-For": fmt.Sprintf("192.0.2.%d", i+1)}
		if code := newAccessRequest(t, h, proxy, spoofed); code != http.StatusOK {
			t.Fatalf("proxied request %d = %d, want 200", i+1, code)
		}
	}
	spoofed := map[string]string{"X-Real-IP": "198.51.100.50", "X-Forwarded-For": "192.0.2.250"}
	if code := newAccessRequest(t, h, proxy, spoofed); code != http.StatusTooManyRequests {
		t.Fatalf("X-Forwarded-For split one client's budget: got %d, want 429", code)
	}
}
