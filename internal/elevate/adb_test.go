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

	// The same refusal through the real port discovery. "Turn it on" is not
	// enough on its own: the owner turned it on, and Android turned it off
	// again after a reboot, a Wi-Fi drop or a move to another access point.
	// Unless the reason says so, the owner's honest answer is "it is on".
	refuseAll := func(a *ADB) {
		a.dial = func(context.Context, string, *adb.Key) (shellConn, error) {
			return nil, errors.New("connect: connection refused")
		}
	}
	whyOff := []string{"Wireless debugging", "reboot", "Wi-Fi disconnects", "different access point"}

	t.Run("the app has found no port", func(t *testing.T) {
		state := filepath.Join(t.TempDir(), "device.json")
		if err := os.WriteFile(state, []byte(`{"level":76}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(StateEnv, state)
		t.Setenv(PortEnv, "")
		a := NewADB(t.TempDir(), "wanctl@test")
		refuseAll(a)
		st := a.Probe(context.Background())
		if st.Available {
			t.Fatal("probe accepted a device with no adbd")
		}
		for _, want := range append(whyOff, "the wanctl app has no current wireless-debugging port", "turn it off and on again") {
			if !strings.Contains(st.Reason, want) {
				t.Errorf("reason = %q\nwant it to say %q", st.Reason, want)
			}
		}
	})

	t.Run("no app to discover the port", func(t *testing.T) {
		t.Setenv(StateEnv, "")
		t.Setenv(PortEnv, "")
		a := NewADB(t.TempDir(), "wanctl@test")
		refuseAll(a)
		st := a.Probe(context.Background())
		for _, want := range whyOff {
			if !strings.Contains(st.Reason, want) {
				t.Errorf("reason = %q\nwant it to say %q", st.Reason, want)
			}
		}
		// Termux and an adb shell have no app watching mDNS, so nothing may
		// be blamed on one.
		if strings.Contains(st.Reason, "wanctl app") {
			t.Errorf("reason = %q, blames an app that is not there", st.Reason)
		}
	})
}

// TestADBRejectedKeyIsNotBlamedOnWirelessDebugging: when adbd answers on the
// discovered port and refuses the key, wireless debugging is demonstrably on.
// The pairing is what has to be redone, and the adb error says so; closing the
// reason with "turn on Wireless debugging" would send the owner to a switch
// that is already on and away from the fix.
func TestADBRejectedKeyIsNotBlamedOnWirelessDebugging(t *testing.T) {
	a := NewADB(t.TempDir(), "wanctl@test")
	a.ports = func() ([]int, string) { return []int{41031, 5555}, "turn on Wireless debugging" }
	a.dial = func(_ context.Context, addr string, _ *adb.Key) (shellConn, error) {
		if addr == "127.0.0.1:41031" {
			return nil, fmt.Errorf("adb: TLS handshake with adbd: remote error: tls: bad certificate (%w: pair again)", adb.ErrKeyRejected)
		}
		return nil, errors.New("connect: connection refused")
	}
	st := a.Probe(context.Background())
	if st.Available {
		t.Fatal("probe accepted a device that refused the key")
	}
	if !strings.Contains(st.Reason, "pair again") {
		t.Errorf("reason = %q, lost the pairing explanation", st.Reason)
	}
	if strings.Contains(st.Reason, "turn on Wireless debugging") {
		t.Errorf("reason = %q, tells the owner to turn on wireless debugging, which is on", st.Reason)
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

// TestADBProbeRedialsADeadCachedConnection is the probe's half of the test
// above. A probe runs `id` on whatever connection an earlier command left open,
// and that socket can die while idle. Reporting the dead socket as "not
// available" is worse than a failed command: the Manager caches the verdict,
// so every elevated command for the next minute is refused while adbd sits
// there listening.
func TestADBProbeRedialsADeadCachedConnection(t *testing.T) {
	first := &stubConn{uid: "uid=2000(shell)"}
	second := &stubConn{uid: "uid=2000(shell)"}
	a := NewADB(t.TempDir(), "wanctl@test")
	a.ports = func() ([]int, string) { return []int{41234}, "hint" }
	dials := 0
	a.dial = func(context.Context, string, *adb.Key) (shellConn, error) {
		dials++
		if dials == 1 {
			return first, nil
		}
		return second, nil
	}
	if st := a.Probe(context.Background()); !st.Available {
		t.Fatalf("first probe = unavailable (%s)", st.Reason)
	}
	first.err = errors.New("connection reset by peer") // dropped while idle

	st := a.Probe(context.Background())
	if !st.Available {
		t.Fatalf("probe reported a dead cached connection as the channel being unavailable: %s", st.Reason)
	}
	if !first.closed {
		t.Error("the dead connection was not closed")
	}
	if dials != 2 || len(second.ran) != 1 || second.ran[0] != "id" {
		t.Errorf("dials=%d, second connection ran %q; want one redial that runs `id`", dials, second.ran)
	}
}

// TestADBProbeDoesNotRetryAFreshConnection: a connection dialed a moment ago
// cannot have gone stale, and when `id` fails on it the failure is the
// diagnosis. Retrying would spend the rest of the probe's budget and could
// replace the device's banner with a dial timeout.
func TestADBProbeDoesNotRetryAFreshConnection(t *testing.T) {
	a := NewADB(t.TempDir(), "wanctl@test")
	a.ports = func() ([]int, string) { return []int{41234}, "hint" }
	dials := 0
	a.dial = func(context.Context, string, *adb.Key) (shellConn, error) {
		dials++
		return &stubConn{err: errors.New("i/o timeout")}, nil
	}
	st := a.Probe(context.Background())
	if st.Available {
		t.Fatal("probe accepted a connection that could not run `id`")
	}
	if dials != 1 {
		t.Errorf("dialed %d times, want 1", dials)
	}
	if !strings.Contains(st.Reason, "`id` failed") {
		t.Errorf("reason = %q, want the failure on the connection itself", st.Reason)
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

	// Age since discovery is not staleness: the app stamps a port once, when
	// mDNS reports it, and never again while it stays up. On 2026-09-09 a
	// PGBM10's state file carried a battery reading under a minute old beside
	// a port stamped eight minutes before it; a 30-minute limit threw such a
	// port away half an hour after discovery, with adbd still listening on it.
	discovered := time.Now().Add(-45 * time.Minute).UTC().Format(time.RFC3339Nano)
	if got := portFromState(write(t, map[string]any{
		"level": 23,
		"adb":   map[string]any{"port": 46321, "updated_at": discovered},
	})); got != 46321 {
		t.Fatalf("port = %d, want 46321: a port discovered 45 minutes ago is still the port", got)
	}

	// A file nothing has maintained for longer than the backstop is not
	// believed: wireless debugging picks a new port every time it is enabled,
	// and something else may hold the old one.
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
