package elevate

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stubSu writes an executable that stands in for a root manager's su. body is
// shell source; the stub is invoked exactly the way the real one is
// (`su -c <command>`), so these tests exercise the real exec path and the real
// argument contract, not a mock of them.
func stubSu(t *testing.T, body string) *Su {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("su channel is Android-only; the stub needs a POSIX shell")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "su")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// /bin/sh stands in for /system/bin/sh, which only exists on a device.
	return &Su{searchPath: []string{path}, stat: os.Stat, shell: "/bin/sh"}
}

// aospSu stands in for AOSP's su: it rejects -c the way the real one does and
// only understands `su WHO COMMAND...`. Measured against the android-29
// google_apis emulator image on 2026-08-14.
const aospSu = `
case "$1" in
  -c) echo "su: invalid uid/gid '-c'" >&2; exit 1 ;;
  root|0) shift; exec "$@" ;;
  *) echo "su: unknown user $1" >&2; exit 1 ;;
esac`

// magiskSu stands in for Magisk/KernelSU/APatch: -c works, and a bare user
// argument is not what wanctl sends it.
const magiskSu = `
case "$1" in
  -c) shift; exec /bin/sh -c "$1" ;;
  *) echo "su: unexpected argument $1" >&2; exit 1 ;;
esac`

// fakeRootID puts an `id` on PATH that answers as root, so a stub su
// delegating to a real shell produces the output a rooted device would.
func fakeRootID(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "id"),
		[]byte("#!/bin/sh\necho 'uid=0(root) gid=0(root) groups=0(root)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestSuProbeReportsRootFromID(t *testing.T) {
	// `su -c id` answering as root is the whole acceptance test for this channel.
	su := stubSu(t, magiskSu)
	fakeRootID(t)

	st := su.Probe(context.Background())
	if !st.Available {
		t.Fatalf("probe = unavailable (%s), want available", st.Reason)
	}
	if !strings.Contains(st.Detail, "uid=0") {
		t.Fatalf("detail = %q, want it to carry the id output", st.Detail)
	}
	if su.form != suFormDashC {
		t.Fatalf("form = %v, want the -c form", su.form)
	}
}

// TestSuProbeFallsBackToTheAOSPInvocation is the emulator's lesson: a su that
// rejects -c is not a broken su, it is AOSP's. Guessing one form would have
// reported "not rooted" on every userdebug build and emulator image.
func TestSuProbeFallsBackToTheAOSPInvocation(t *testing.T) {
	su := stubSu(t, aospSu)
	fakeRootID(t)

	st := su.Probe(context.Background())
	if !st.Available {
		t.Fatalf("probe = unavailable (%s); an AOSP su was mistaken for no root at all", st.Reason)
	}
	if su.form != suFormUserSh {
		t.Fatalf("form = %v, want the `su root sh -c` form", su.form)
	}

	// And the form it learned is the one Run then uses.
	var buf bytes.Buffer
	if _, err := su.Run(context.Background(), `echo "a b"`, "", &buf); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(buf.String()); got != "a b" {
		t.Fatalf("Run output = %q, want the command to have survived intact", got)
	}
}

// TestSuProbeReportsTheLastFailureWhenNoFormWorks keeps a device with a broken
// su from being reported with an empty reason.
func TestSuProbeReportsTheLastFailureWhenNoFormWorks(t *testing.T) {
	su := stubSu(t, `echo "su: access denied" >&2; exit 1`)
	st := su.Probe(context.Background())
	if st.Available {
		t.Fatal("probe accepted a su that failed in every form")
	}
	if !strings.Contains(st.Reason, "access denied") {
		t.Fatalf("reason = %q, want su's own complaint", st.Reason)
	}
}

func TestSuProbeRejectsSuThatIsNotRoot(t *testing.T) {
	// A su that exits 0 without yielding root must not be reported as a working
	// channel: every command sent through it afterwards would silently run
	// unprivileged, which is the exact failure this package exists to prevent.
	su := stubSu(t, `echo 'uid=2000(shell) gid=2000(shell)'; exit 0`)
	st := su.Probe(context.Background())
	if st.Available {
		t.Fatal("probe accepted a su that did not yield uid=0")
	}
	if !strings.Contains(st.Reason, "did not yield root") {
		t.Fatalf("reason = %q, want it to name the missing root", st.Reason)
	}
}

func TestSuProbeReportsRefusal(t *testing.T) {
	su := stubSu(t, `echo 'permission denied' >&2; exit 1`)
	st := su.Probe(context.Background())
	if st.Available {
		t.Fatal("probe accepted a su that exited non-zero")
	}
	if !strings.Contains(st.Reason, "permission denied") {
		t.Fatalf("reason = %q, want it to carry su's own complaint", st.Reason)
	}
}

func TestSuProbeReportsMissingBinary(t *testing.T) {
	su := &Su{searchPath: []string{filepath.Join(t.TempDir(), "nope")}, stat: os.Stat}
	st := su.Probe(context.Background())
	if st.Available {
		t.Fatal("probe accepted a device with no su binary")
	}
	if !strings.Contains(st.Reason, "rooted") {
		t.Fatalf("reason = %q, want it to say the device is not rooted", st.Reason)
	}
}

func TestSuRunPassesCommandAsOneArgument(t *testing.T) {
	// The quoting hazard v0.1.12 documented for `exec` must not reappear here:
	// su receives the command as a single argv slot and hands it to a shell
	// itself. A command with spaces, quotes and a semicolon has to survive.
	su := stubSu(t, `printf '%s' "$2"`)
	su.form = suFormDashC
	var buf bytes.Buffer
	code, err := su.Run(context.Background(), `echo "a b"; ls -d /x`, "", &buf)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got := buf.String(); got != `echo "a b"; ls -d /x` {
		t.Fatalf("su received %q, want the command intact in one argument", got)
	}
}

func TestSuRunPropagatesExitCode(t *testing.T) {
	su := stubSu(t, `exit 42`)
	su.form = suFormDashC
	code, err := su.Run(context.Background(), "whatever", "", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if code != 42 {
		t.Fatalf("exit = %d, want 42", code)
	}
}

func TestSuRunUsesCwdWithoutTouchingTheCommand(t *testing.T) {
	// cwd travels as a process attribute, never as a `cd ... &&` prefix, so a
	// directory with a space or a quote in it cannot corrupt the command.
	dir := filepath.Join(t.TempDir(), "a dir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	su := stubSu(t, `[ "$1" = "-c" ] && exec /bin/pwd`)
	su.form = suFormDashC
	var buf bytes.Buffer
	if _, err := su.Run(context.Background(), "pwd", dir, &buf); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(buf.String())
	// macOS resolves TempDir through /private; compare on the suffix.
	if !strings.HasSuffix(got, "a dir") {
		t.Fatalf("cwd = %q, want it to end in %q", got, "a dir")
	}
}

func TestSuProbeTimesOutOnAConsentDialogNobodyTaps(t *testing.T) {
	if testing.Short() {
		t.Skip("sleeps")
	}
	su := stubSu(t, `sleep 30`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // stand in for the deadline, without waiting 20s for it
	st := su.Probe(ctx)
	if st.Available {
		t.Fatal("probe reported available while su never answered")
	}
}
