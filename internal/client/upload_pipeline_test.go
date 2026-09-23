package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wanctl/internal/agent"
	"wanctl/internal/policy"
	"wanctl/internal/relay"
)

// hostileCarrier sits between both ends and a real relay. It delays uploads by
// random amounts so they overtake each other, and now and then lets the relay
// accept an upload but reports 502 to the writer, as a carrier that loses the
// response would. The writer must retry, and the relay must not queue the
// retried bytes twice.
type hostileCarrier struct {
	next http.Handler

	inFlight    atomic.Int32
	maxInFlight atomic.Int32
	lostReplies atomic.Int32
	mu          sync.Mutex
	uploads     int
}

func (h *hostileCarrier) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/h/up" {
		h.next.ServeHTTP(w, r)
		return
	}
	n := h.inFlight.Add(1)
	defer h.inFlight.Add(-1)
	for {
		m := h.maxInFlight.Load()
		if n <= m || h.maxInFlight.CompareAndSwap(m, n) {
			break
		}
	}
	// Every upload takes a while, as over a real link, so a retry lands in
	// the middle of the stream rather than after it has finished.
	jitter, _ := rand.Int(rand.Reader, big.NewInt(40))
	time.Sleep(time.Duration(50+jitter.Int64()) * time.Millisecond)

	h.mu.Lock()
	h.uploads++
	lose := h.uploads%7 == 0 && r.URL.Query().Get("seq") != ""
	h.mu.Unlock()
	if lose {
		rec := httptest.NewRecorder()
		h.next.ServeHTTP(rec, r)
		if rec.Code == http.StatusOK {
			h.lostReplies.Add(1)
			http.Error(w, "carrier lost the reply", http.StatusBadGateway)
			return
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		w.Write(rec.Body.Bytes())
		return
	}
	h.next.ServeHTTP(w, r)
}

func TestPipelinedUploadsSurviveReorderingAndLostReplies(t *testing.T) {
	carrier := &hostileCarrier{next: relay.New(relay.EnvTokenStore("tok:alice")).Handler()}
	srv := httptest.NewServer(carrier)
	defer srv.Close()

	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	ag, err := agent.New(agent.Options{RelayURL: srv.URL, Token: "tok", Name: "home-pc", AutoYes: true, Transport: "http", Mode: policy.ModeBypass})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)
	time.Sleep(300 * time.Millisecond)

	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", srv.URL)
	t.Setenv("WANCTL_TOKEN", "tok")
	t.Setenv("WANCTL_TRANSPORT", "http")
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	trustServer(t, c, "home-pc")

	payload := make([]byte, 24<<20+12345)
	rand.Read(payload)
	local := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(local, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(t.TempDir(), "remote.bin")
	if err := c.Push(context.Background(), "home-pc", local, remote); err != nil {
		t.Fatalf("push: %v", err)
	}
	got, _ := os.ReadFile(remote)
	if !bytes.Equal(got, payload) {
		t.Fatalf("push arrived corrupted: %d bytes, want %d", len(got), len(payload))
	}
	back := filepath.Join(t.TempDir(), "back.bin")
	if err := c.Pull(context.Background(), "home-pc", remote, back); err != nil {
		t.Fatalf("pull: %v", err)
	}
	got, _ = os.ReadFile(back)
	if !bytes.Equal(got, payload) {
		t.Fatalf("pull arrived corrupted: %d bytes, want %d", len(got), len(payload))
	}
	if m := carrier.maxInFlight.Load(); m < 2 {
		t.Fatalf("at most %d upload in flight: writes were not pipelined", m)
	}
	if carrier.lostReplies.Load() == 0 {
		t.Fatal("no reply was lost, so retries were not exercised")
	}
}
