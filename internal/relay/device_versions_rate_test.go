package relay

import (
	"context"
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

type rateVersionRecord struct {
	ns, id, w, o string
	at           time.Time
}

// Port of the rate subtest of TestInvariant6VersionsValidationIsolationAndRate.
// Send a burst rather than waiting for an ACK/write after every message: the
// required behavior deliberately drops intermediate pending values.
func TestInvariant6VersionsValidationIsolationAndRate(t *testing.T) {
	t.Run("rate", func(t *testing.T) {
		if deviceVersionWriteInterval != 10*time.Second {
			t.Fatal("production rate changed")
		}
		records := make(chan rateVersionRecord, 512)
		store := &versionTestStore{write: func(_ context.Context, ns, id, w, o string, _ bool) error {
			records <- rateVersionRecord{ns, id, w, o, time.Now()}
			return nil
		}}
		r := New(EnvTokenStore("alice:alice"))
		r.versionWriteInterval = 100 * time.Millisecond
		r.SetAdmin(store)
		srv := httptest.NewServer(http.HandlerFunc(r.handleAgent))
		defer srv.Close()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		conn, _, err := wsconn.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), admission.Header("alice"))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		enc := json.NewEncoder(conn)
		recv := func() rateVersionRecord {
			t.Helper()
			select {
			case p := <-records:
				return p
			case <-ctx.Done():
				t.Fatal("flush timeout")
				return rateVersionRecord{}
			}
		}
		reg := versionRegistration()
		reg["wanctl_version"] = "v0.26.0"
		if err = enc.Encode(reg); err != nil {
			t.Fatal(err)
		}
		first := recv()
		for i := 0; i < 200; i++ {
			if err = enc.Encode(map[string]string{"op": "versions", "namespace": "bob", "device": "victim", "wanctl_version": fmt.Sprintf("v1.0.%d", i+2), "opencu_version": "0.4.1"}); err != nil {
				t.Fatal(err)
			}
		}
		previous := first
		for {
			p := recv()
			if p.ns != "alice" || p.id != versionTestID {
				t.Fatalf("identity: %+v", p)
			}
			if p.at.Sub(previous.at) < r.versionWriteInterval-time.Millisecond {
				t.Fatalf("unbounded writes: %s", p.at.Sub(previous.at))
			}
			previous = p
			if p.w == "v1.0.201" {
				break
			}
		}
		if store.calls.Load() >= 201 {
			t.Fatal("persisted every report")
		}
		before := store.calls.Load()
		for i := 0; i < 100; i++ {
			if err = enc.Encode(map[string]string{"op": "versions", "wanctl_version": "v1.0.201", "opencu_version": "0.4.1"}); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(2 * r.versionWriteInterval)
		if store.calls.Load() != before {
			t.Fatal("duplicates persisted")
		}
	})
}

func TestVersionWriteFailuresAreRateLimited(t *testing.T) {
	records := make(chan time.Time, 10)
	r := New(EnvTokenStore("alice:alice"))
	r.versionWriteInterval = 40 * time.Millisecond
	r.SetAdmin(&versionTestStore{write: func(context.Context, string, string, string, string, bool) error {
		records <- time.Now()
		return fmt.Errorf("store unavailable")
	}})
	r.queueDeviceVersions("alice", "dev", "one", "v1.0.0", "", true)()
	first := <-records
	for i := 0; i < 100; i++ {
		r.queueDeviceVersions("alice", "dev", "one", fmt.Sprintf("v1.0.%d", i+1), "", true)()
	}
	select {
	case second := <-records:
		if second.Sub(first) < r.versionWriteInterval-time.Millisecond {
			t.Fatal("failure retried without backoff")
		}
	case <-time.After(time.Second):
		t.Fatal("latest pending report was not flushed")
	}
	waitVersion(t, func() bool { r.versionsMu.Lock(); defer r.versionsMu.Unlock(); return !r.versions["alice/dev"].running })
}
