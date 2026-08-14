package elevate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wanctl/internal/adb"
)

// stubConn stands in for a live adbd connection.
type stubConn struct {
	uid      string
	ran      []string
	code     int
	err      error
	closed   bool
	failOnce bool
}

func (s *stubConn) Shell(_ context.Context, command string, out io.Writer) (int, error) {
	s.ran = append(s.ran, command)
	if s.failOnce {
		s.failOnce = false
		return -1, errors.New("connection reset by peer")
	}
	if s.err != nil {
		return -1, s.err
	}
	if command == "id" {
		fmt.Fprintln(out, s.uid)
		return 0, nil
	}
	fmt.Fprintln(out, "ran: "+command)
	return s.code, nil
}

func (s *stubConn) Close() error { s.closed = true; return nil }

// newTestADB builds a channel whose dialer answers on exactly one port.
func newTestADB(t *testing.T, port int, conn *stubConn, dialErr error) *ADB {
	t.Helper()
	a := NewADB(t.TempDir(), "wanctl@test")
	a.ports = func() ([]int, string) { return []int{port}, "turn on wireless debugging" }
	a.dial = func(_ context.Context, addr string, _ *adb.Key) (shellConn, error) {
		if dialErr != nil {
			return nil, dialErr
		}
		if want := fmt.Sprintf("127.0.0.1:%d", port); addr != want {
			t.Errorf("dialed %q, want %q", addr, want)
		}
		return conn, nil
	}
	return a
}

func TestADBProbeAcceptsShellUID(t *testing.T) {
	a := newTestADB(t, 41234, &stubConn{uid: "uid=2000(shell) gid=2000(shell) context=u:r:shell:s0"}, nil)
	st := a.Probe(context.Background())
	if !st.Available {
		t.Fatalf("probe = unavailable (%s)", st.Reason)
	}
	if !strings.Contains(st.Detail, "uid=2000") {
		t.Fatalf("detail = %q, want the id output", st.Detail)
	}
}

// TestADBProbeRejectsAnAppUID is the trap this channel exists to avoid. If the
// thing answering on that port runs as an app, connecting to it is not an
// elevation at all, and every command afterwards would run with no more
// privilege than the agent already had — silently.
func TestADBProbeRejectsAnAppUID(t *testing.T) {
	a := newTestADB(t, 41234, &stubConn{uid: "uid=10601(u0_a601) gid=10601(u0_a601)"}, nil)
	st := a.Probe(context.Background())
	if st.Available {
		t.Fatal("probe accepted an adbd running as an app uid")
	}
	if !strings.Contains(st.Reason, "app uid") {
		t.Fatalf("reason = %q, want it to name the wrong uid", st.Reason)
	}
}

func TestADBProbeExplainsWhenNothingIsListening(t *testing.T) {
	a := newTestADB(t, 5555, nil, errors.New("connection refused"))
	st := a.Probe(context.Background())
	if st.Available {
		t.Fatal("probe accepted a device with no adbd")
	}
	if !strings.Contains(st.Reason, "Wireless debugging") && !strings.Contains(st.Reason, "wireless debugging") {
		t.Fatalf("reason = %q, want it to say what the owner should turn on", st.Reason)
	}
}

// TestADBPendingAuthorizationStopsTheSearch: when adbd is waiting for someone
// to tap Allow, trying the next port would bury the one message that matters.
func TestADBPendingAuthorizationStopsTheSearch(t *testing.T) {
	a := NewADB(t.TempDir(), "wanctl@test")
	a.ports = func() ([]int, string) { return []int{40000, 5555}, "hint" }
	dialed := 0
	a.dial = func(context.Context, string, *adb.Key) (shellConn, error) {
		dialed++
		return nil, adb.ErrPublicKeyPending
	}
	st := a.Probe(context.Background())
	if st.Available {
		t.Fatal("probe reported available while the key is unapproved")
	}
	if dialed != 1 {
		t.Fatalf("dialed %d ports, want 1 (a pending prompt must not be buried)", dialed)
	}
	if !strings.Contains(st.Reason, "allow wanctl's key") {
		t.Fatalf("reason = %q, want it to name the tap-to-allow prompt", st.Reason)
	}
}

func TestADBRunPrefixesCwdWithoutBreakingTheCommand(t *testing.T) {
	conn := &stubConn{uid: "uid=2000(shell)"}
	a := newTestADB(t, 41234, conn, nil)
	if _, err := a.Run(context.Background(), `echo "a b"`, "/data/local/a dir", io.Discard); err != nil {
		t.Fatal(err)
	}
	got := conn.ran[len(conn.ran)-1]
	if want := `cd '/data/local/a dir' && echo "a b"`; got != want {
		t.Fatalf("ran %q, want %q", got, want)
	}
}

// TestADBRunRetriesOnceOnADeadConnection: wireless debugging drops the socket
// when the screen locks on some ROMs, and a cached dead connection must not
// turn into a spurious command failure.
func TestADBRunRetriesOnceOnADeadConnection(t *testing.T) {
	first := &stubConn{uid: "uid=2000(shell)", failOnce: true}
	second := &stubConn{uid: "uid=2000(shell)"}
	a := NewADB(t.TempDir(), "wanctl@test")
	a.ports = func() ([]int, string) { return []int{41234}, "hint" }
	n := 0
	a.dial = func(context.Context, string, *adb.Key) (shellConn, error) {
		n++
		if n == 1 {
			return first, nil
		}
		return second, nil
	}
	if _, err := a.Run(context.Background(), "id", "", io.Discard); err != nil {
		t.Fatalf("run did not recover from a dropped connection: %v", err)
	}
	if !first.closed {
		t.Fatal("the dead connection was not closed")
	}
	if len(second.ran) != 1 {
		t.Fatalf("retry ran %d commands on the new connection, want 1", len(second.ran))
	}
}

func TestPortFromState(t *testing.T) {
	write := func(t *testing.T, v any) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "device.json")
		data, _ := json.Marshal(v)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	fresh := time.Now().UTC().Format(time.RFC3339Nano)

	if got := portFromState(write(t, map[string]any{
		"adb": map[string]any{"port": 37123, "updated_at": fresh},
	})); got != 37123 {
		t.Fatalf("port = %d, want 37123", got)
	}

	// A stale port is a wrong answer, not an old one: wireless debugging picks
	// a new port every time it is enabled, and something else may hold the old.
	old := time.Now().Add(-2 * maxADBPortAge).UTC().Format(time.RFC3339Nano)
	if got := portFromState(write(t, map[string]any{
		"adb": map[string]any{"port": 37123, "updated_at": old},
	})); got != 0 {
		t.Fatalf("port = %d, want 0 for a stale entry", got)
	}

	// A state file carrying only battery (every build before this one) must not
	// break, and must not produce a port.
	if got := portFromState(write(t, map[string]any{"level": 76})); got != 0 {
		t.Fatalf("port = %d, want 0", got)
	}
	if got := portFromState(filepath.Join(t.TempDir(), "absent.json")); got != 0 {
		t.Fatalf("port = %d for a missing file, want 0", got)
	}
	if got := portFromState(""); got != 0 {
		t.Fatalf("port = %d for an unset path, want 0", got)
	}
}
