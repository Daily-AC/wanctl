package relay

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wanctl/internal/admission"
	"wanctl/internal/transport"
	"wanctl/internal/wsconn"
)

const versionTestID = "12345678-1234-4234-8234-123456789abc"

type versionTestStore struct {
	noopAdmin
	write       func(context.Context, string, string, string, string, bool) error
	identityErr error
	calls       atomic.Int32
}

func (s *versionTestStore) RegisterDevice(ns, id, name, fp string) (bool, error) {
	return false, s.identityErr
}
func (s *versionTestStore) RecordDeviceVersions(ctx context.Context, ns, id, w, o string, reported bool) error {
	s.calls.Add(1)
	return s.write(ctx, ns, id, w, o, reported)
}
func versionRegistration() map[string]any {
	return map[string]any{"op": "register", "device": versionTestID, "device_id": versionTestID, "name": "versions", "fingerprint": transport.Fingerprint([]byte("versions")), "inst": "one"}
}
func versionPoll(r *Relay, ns string, reg map[string]any) *httptest.ResponseRecorder {
	q := url.Values{}
	for k, v := range reg {
		q.Set(k, fmt.Sprint(v))
	}
	q.Set("fp", fmt.Sprint(reg["fingerprint"]))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/h/poll?"+q.Encode(), nil).WithContext(ctx)
	admission.SetBearer(req, ns)
	rec := httptest.NewRecorder()
	r.handleHPoll(rec, req)
	return rec
}
func waitVersion(t *testing.T, condition func() bool) {
	t.Helper()
	until := time.Now().Add(7 * time.Second)
	for !condition() {
		if time.Now().After(until) {
			t.Fatal("version condition timed out")
		}
		time.Sleep(time.Millisecond)
	}
}
func versionIdle(r *Relay, ns string) bool {
	r.versionsMu.Lock()
	defer r.versionsMu.Unlock()
	s := r.versions[ns+"/"+versionTestID]
	return s != nil && !s.running
}

func TestVersionStoreFailureNeverRefusesRegistration(t *testing.T) {
	for _, tr := range []string{"ws", "http"} {
		for _, fields := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fields=%v", tr, fields), func(t *testing.T) {
				var output bytes.Buffer
				old := log.Writer()
				log.SetOutput(&output)
				defer log.SetOutput(old)
				var fail atomic.Bool
				fail.Store(true)
				store := &versionTestStore{write: func(context.Context, string, string, string, string, bool) error {
					if fail.Load() {
						return errors.New("SECRET driver detail")
					}
					return nil
				}}
				r := New(EnvTokenStore("alice:alice"))
				r.SetAdmin(store)
				srv := httptest.NewServer(http.HandlerFunc(r.handleAgent))
				defer srv.Close()
				reg := versionRegistration()
				if fields {
					reg["wanctl_version"] = "v1.2.3"
				}
				register := func() {
					t.Helper()
					want := store.calls.Load() + 1
					if tr == "http" {
						if rec := versionPoll(r, "alice", reg); rec.Code != 200 {
							t.Fatalf("poll %d", rec.Code)
						}
					} else {
						ctx, cancel := context.WithTimeout(context.Background(), time.Second)
						defer cancel()
						conn, _, err := wsconn.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), admission.Header("alice"))
						if err != nil {
							t.Fatal(err)
						}
						// Keep each accepted connection alive until its metadata update finishes.
						defer conn.Close()
						if err := json.NewEncoder(conn).Encode(reg); err != nil {
							t.Fatal(err)
						}
						waitVersion(t, func() bool { return store.calls.Load() >= want && versionIdle(r, "alice") })
						if !r.deviceLive("alice", versionTestID) {
							t.Fatal("WS offline after metadata failure")
						}
						return
					}
					waitVersion(t, func() bool { return store.calls.Load() >= want && versionIdle(r, "alice") })
					if !r.deviceLive("alice", versionTestID) {
						t.Fatal("HTTP offline after metadata failure")
					}
				}
				register()
				register()
				if got := strings.Count(output.String(), "metadata write failed"); got != 1 {
					t.Fatalf("failure streak logged %d times", got)
				}
				if strings.Contains(output.String(), "SECRET") || strings.Contains(output.String(), versionTestID) {
					t.Fatal("unsafe log contents")
				}
				fail.Store(false)
				register()
				fail.Store(true)
				reg["wanctl_version"] = "v2.0.0"
				register()
				if got := strings.Count(output.String(), "metadata write failed"); got != 2 {
					t.Fatalf("new failure streak logged %d times", got)
				}
			})
		}
	}
}

func TestSlowVersionStoreDoesNotBlockAnotherNamespace(t *testing.T) {
	entered := make(chan context.Context, 1)
	store := &versionTestStore{write: func(ctx context.Context, ns, id, w, o string, reported bool) error {
		if ns == "alice" {
			entered <- ctx
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}}
	r := New(EnvTokenStore("alice:alice,bob:bob"))
	r.SetAdmin(store)
	reg := versionRegistration()
	reg["wanctl_version"] = "v1.0.0"
	start := time.Now()
	if rec := versionPoll(r, "alice", reg); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	ctx := <-entered
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > deviceVersionWriteTimeout {
		t.Fatal("unbounded metadata context")
	}
	if rec := versionPoll(r, "bob", reg); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	waitVersion(t, func() bool { return versionIdle(r, "bob") })
	if time.Since(start) > time.Second {
		t.Fatal("slow metadata blocked registration")
	}
	if !r.deviceLive("alice", versionTestID) || !r.deviceLive("bob", versionTestID) {
		t.Fatal("slow metadata affected liveness")
	}
	waitVersion(t, func() bool { return versionIdle(r, "alice") })
}

func TestVersionWritesKeepLatestAcceptedReport(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var writes []string
	store := &versionTestStore{write: func(ctx context.Context, ns, id, w, o string, reported bool) error {
		if w == "v1.0.0" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		mu.Lock()
		writes = append(writes, w)
		mu.Unlock()
		return nil
	}}
	r := New(EnvTokenStore("alice:alice"))
	r.SetAdmin(store)
	reg := versionRegistration()
	reg["wanctl_version"] = "v1.0.0"
	versionPoll(r, "alice", reg)
	<-entered
	reg["wanctl_version"] = "v2.0.0"
	versionPoll(r, "alice", reg)
	reg["inst"] = "two"
	reg["wanctl_version"] = "v3.0.0"
	versionPoll(r, "alice", reg)
	// A retired instance must not enqueue its stale report.
	reg["inst"] = "one"
	reg["wanctl_version"] = "v0.0.1"
	if rec := versionPoll(r, "alice", reg); rec.Code != 409 {
		t.Fatalf("retired poll %d", rec.Code)
	}
	close(release)
	waitVersion(t, func() bool { return versionIdle(r, "alice") })
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(writes) != "[v1.0.0 v3.0.0]" {
		t.Fatalf("writes %v", writes)
	}
}

type versionSQLDriver struct {
	writes, versions, tx, unbounded, locked atomic.Int32
	relay                                   *Relay
}

func (d *versionSQLDriver) Open(string) (driver.Conn, error) { return &versionSQLConn{d}, nil }

type versionSQLConn struct{ d *versionSQLDriver }

func (c *versionSQLConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (c *versionSQLConn) Close() error              { return nil }
func (c *versionSQLConn) Begin() (driver.Tx, error) { c.d.tx.Add(1); return versionSQLTx{}, nil }

type versionSQLTx struct{}

func (versionSQLTx) Commit() error   { return nil }
func (versionSQLTx) Rollback() error { return nil }
func (c *versionSQLConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(q, "SELECT EXISTS") {
		return &adminRows{columns: []string{"exists"}, values: [][]driver.Value{{true}}}, nil
	}
	return nil, errors.New("unexpected query")
}
func (c *versionSQLConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if strings.HasPrefix(q, "INSERT INTO devices") || strings.HasPrefix(q, "UPDATE devices") {
		c.d.writes.Add(1)
	}
	if strings.HasPrefix(q, "UPDATE devices SET wanctl_version") {
		c.d.versions.Add(1)
		if c.d.relay.registrationMu.TryLock() {
			c.d.relay.registrationMu.Unlock()
		} else {
			c.d.locked.Add(1)
		}
		if _, ok := ctx.Deadline(); !ok {
			c.d.unbounded.Add(1)
		}
	}
	return driver.RowsAffected(1), nil
}
func TestIdenticalPollsWriteVersionsOnce(t *testing.T) {
	state := &versionSQLDriver{}
	name := "versions-" + randHex(8)
	sql.Register(name, state)
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := New(EnvTokenStore("alice:alice"))
	state.relay = r
	r.SetAdmin(&PGStore{db: db})
	reg := versionRegistration()
	reg["wanctl_version"] = "v1.0.0"
	for i := 0; i < 4; i++ {
		if rec := versionPoll(r, "alice", reg); rec.Code != 200 {
			t.Fatal(rec.Code)
		}
		waitVersion(t, func() bool { return versionIdle(r, "alice") })
	}
	if state.versions.Load() != 1 || state.writes.Load() != 5 || state.tx.Load() != 4 || state.unbounded.Load() != 0 || state.locked.Load() != 0 {
		t.Fatalf("versions=%d writes=%d identity tx=%d unbounded=%d", state.versions.Load(), state.writes.Load(), state.tx.Load(), state.unbounded.Load())
	}
	reg["inst"] = "two"
	versionPoll(r, "alice", reg)
	waitVersion(t, func() bool { return versionIdle(r, "alice") })
	reg["wanctl_version"] = "v2.0.0"
	versionPoll(r, "alice", reg)
	waitVersion(t, func() bool { return versionIdle(r, "alice") })
	if state.versions.Load() != 3 {
		t.Fatalf("instance/value change writes=%d", state.versions.Load())
	}
}

func TestIdentityFailureStillRefusesRegistration(t *testing.T) {
	for _, tr := range []string{"ws", "http"} {
		t.Run(tr, func(t *testing.T) {
			store := &versionTestStore{identityErr: errors.New("identity failed"), write: func(context.Context, string, string, string, string, bool) error {
				t.Error("metadata after identity refusal")
				return nil
			}}
			r := New(EnvTokenStore("alice:alice"))
			r.SetAdmin(store)
			reg := versionRegistration()
			if tr == "http" {
				if rec := versionPoll(r, "alice", reg); rec.Code != 409 {
					t.Fatalf("HTTP %d", rec.Code)
				}
			} else {
				srv := httptest.NewServer(http.HandlerFunc(r.handleAgent))
				defer srv.Close()
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				conn, _, err := wsconn.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), admission.Header("alice"))
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				if err := json.NewEncoder(conn).Encode(reg); err != nil {
					t.Fatal(err)
				}
				var msg json.RawMessage
				if err := json.NewDecoder(conn).Decode(&msg); err == nil {
					t.Fatal("WS accepted identity failure")
				}
			}
			if r.deviceLive("alice", versionTestID) || store.calls.Load() != 0 {
				t.Fatal("identity refusal bypassed")
			}
		})
	}
}

// WebSocket devices report OpenCU installs on the existing control stream.
// A displaced connection must not overwrite the new instance's metadata.
func TestLiveVersionReportWithoutReconnect(t *testing.T) {
	var mu sync.Mutex
	var latest string
	store := &versionTestStore{write: func(_ context.Context, ns, id, w, o string, reported bool) error {
		mu.Lock()
		latest = o
		mu.Unlock()
		return nil
	}}
	r := New(EnvTokenStore("alice:alice"))
	r.SetAdmin(store)
	srv := httptest.NewServer(http.HandlerFunc(r.handleAgent))
	defer srv.Close()
	conn, _, err := wsconn.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"), admission.Header("alice"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	enc := json.NewEncoder(conn)
	reg := versionRegistration()
	reg["wanctl_version"] = "v0.26.0"
	reg["opencu_version"] = "0.3.0"
	if err = enc.Encode(reg); err != nil {
		t.Fatal(err)
	}
	waitVersion(t, func() bool { mu.Lock(); defer mu.Unlock(); return latest == "0.3.0" })
	if err = enc.Encode(map[string]string{"op": "versions", "wanctl_version": "v0.26.0", "opencu_version": "0.4.0"}); err != nil {
		t.Fatal(err)
	}
	waitVersion(t, func() bool { mu.Lock(); defer mu.Unlock(); return latest == "0.4.0" })
	if !r.deviceLive("alice", versionTestID) {
		t.Fatal("version refresh disconnected device")
	}
}
