package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wanctl/internal/admission"
	"wanctl/internal/wsconn"
)

// Disconnected devices do not leave version cache records behind.
func TestDisconnectedRegistrationsLeaveNoRelayMemory(t *testing.T) {
	store := &versionTestStore{write: func(context.Context, string, string, string, string, bool) error { return nil }}
	r := New(EnvTokenStore("alice:alice"))
	r.versionWriteInterval = time.Millisecond
	r.SetAdmin(store)
	srv := httptest.NewServer(http.HandlerFunc(r.handleAgent))
	defer srv.Close()

	const rounds = 200
	for i := 0; i < rounds; i++ {
		device := fmt.Sprintf("legacy-%04d-%s", i, strings.Repeat("x", 200))
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		conn, _, err := wsconn.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), admission.Header("alice"))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if err := json.NewEncoder(conn).Encode(map[string]any{"op": "register", "device": device, "inst": fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
		waitVersion(t, func() bool { return r.deviceLive("alice", device) })
		conn.Close()
		cancel()
		waitVersion(t, func() bool { return !r.deviceLive("alice", device) })
	}

	waitVersion(t, func() bool { r.versionsMu.Lock(); defer r.versionsMu.Unlock(); return len(r.versions) == 0 })
	r.versionsMu.Lock()
	entries, keyBytes := len(r.versions), 0
	for k := range r.versions {
		keyBytes += len(k)
	}
	r.versionsMu.Unlock()
	if entries != 0 {
		t.Fatalf("after %d connect/register/disconnect rounds by one account, with no device online, the relay still holds %d version records keyed by %d bytes of device names (unbounded, never pruned)",
			rounds, entries, keyBytes)
	}
}

// Exercise the same lifecycle with the production metadata store.
func TestDisconnectedRegistrationsLeaveNoRelayMemoryPG(t *testing.T) {
	p, alice, _ := disableTestStore(t)
	r := New(p)
	r.versionWriteInterval = time.Millisecond
	r.SetAdmin(p)
	srv := httptest.NewServer(http.HandlerFunc(r.handleAgent))
	defer srv.Close()
	const rounds = 50
	for i := 0; i < rounds; i++ {
		b := make([]byte, 100)
		rand.Read(b)
		device := fmt.Sprintf("legacy-%04d-%s", i, hex.EncodeToString(b)) // distinct valid names
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, _, err := wsconn.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), admission.Header(alice))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if err := json.NewEncoder(conn).Encode(map[string]any{"op": "register", "device": device, "inst": fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
		waitVersion(t, func() bool { return r.deviceLive("alice", device) })
		conn.Close()
		cancel()
		waitVersion(t, func() bool { return !r.deviceLive("alice", device) })
	}
	waitVersion(t, func() bool { r.versionsMu.Lock(); defer r.versionsMu.Unlock(); return len(r.versions) == 0 })
	r.versionsMu.Lock()
	entries, keyBytes := len(r.versions), 0
	for k := range r.versions {
		keyBytes += len(k)
	}
	r.versionsMu.Unlock()
	if entries != 0 {
		t.Fatalf("PG store: after %d rounds the relay holds %d version records keyed by %d bytes", rounds, entries, keyBytes)
	}
}

func TestRegistrationRejectsInvalidDeviceNames(t *testing.T) {
	for _, device := range []string{strings.Repeat("x", 256), "bad\nname", "bad\x00name"} {
		t.Run(fmt.Sprintf("%q", device), func(t *testing.T) {
			r := New(EnvTokenStore("alice:alice"))
			srv := httptest.NewServer(http.HandlerFunc(r.handleAgent))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			conn, _, err := wsconn.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), admission.Header("alice"))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := json.NewEncoder(conn).Encode(map[string]any{"op": "register", "device": device}); err != nil {
				t.Fatal(err)
			}
			conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			var reply json.RawMessage
			_ = json.NewDecoder(conn).Decode(&reply)
			if r.deviceLive("alice", device) {
				t.Fatal("invalid device registered")
			}
		})
	}
}

func TestVersionWritesThrottleAndKeepNewestReport(t *testing.T) {
	type write struct {
		version string
		at      time.Time
	}
	writes := make(chan write, 32)
	store := &versionTestStore{write: func(_ context.Context, _, _, w, _ string, _ bool) error {
		writes <- write{w, time.Now()}
		return nil
	}}
	r := New(EnvTokenStore("alice:alice"))
	r.versionWriteInterval = time.Second
	r.SetAdmin(store)
	reg := versionRegistration()
	reg["wanctl_version"] = "v1.0.0"
	versionPoll(r, "alice", reg)
	first := <-writes
	for i := 1; i <= 10; i++ {
		reg["wanctl_version"] = fmt.Sprintf("v1.0.%d", i)
		versionPoll(r, "alice", reg)
	}
	// Disconnect with a newer report still waiting behind the interval.
	req := httptest.NewRequest(http.MethodPost, "/h/deregister?device="+versionTestID, nil)
	admission.SetBearer(req, "alice")
	rec := httptest.NewRecorder()
	r.handleHDeregister(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	select {
	case next := <-writes:
		if next.at.Sub(first.at) < 950*time.Millisecond || next.version != "v1.0.10" {
			t.Fatalf("next write = %s after %s; want newest after minimum interval", next.version, next.at.Sub(first.at))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("newest pending report was not written")
	}
	waitVersion(t, func() bool { r.versionsMu.Lock(); defer r.versionsMu.Unlock(); return len(r.versions) == 0 })
	if len(writes) != 0 {
		t.Fatal("intermediate reports were written")
	}
}
