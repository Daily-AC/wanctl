//go:build !windows

package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func newTestSession(t *testing.T) *ShellSession {
	t.Helper()
	s, err := NewShellSession("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// parkChild is a command that records the pid of a real grandchild and then
// blocks, so a test can watch the device side rather than trusting the return
// value of the thing it is testing.
func parkChild(pidFile string, seconds int) string {
	return fmt.Sprintf("/bin/sh -c 'echo $$ > %s; exec /bin/sleep %d'", pidFile, seconds)
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil && strings.TrimSpace(string(b)) != "" {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(b)))
			if convErr != nil {
				t.Fatalf("pid file %q: %v", b, convErr)
			}
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the command never wrote its pid to %s", path)
	return 0
}

func processGone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return errors.Is(err, syscall.ESRCH)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return syscall.Kill(pid, 0) != nil
}

type execResult struct {
	code int
	err  error
}

func runAsync(s *ShellSession, ctx context.Context, command string) <-chan execResult {
	done := make(chan execResult, 1)
	go func() {
		code, err := s.ExecInDirContext(ctx, command, "", io.Discard)
		done <- execResult{code: code, err: err}
	}()
	return done
}

func awaitExec(t *testing.T, ch <-chan execResult, within time.Duration) execResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(within):
		t.Fatal("the command never returned")
		return execResult{}
	}
}

// A cancel must stop everything the submitted line set in motion, not just the
// process that happened to be running. Killing the child and keeping the shell
// left the rest of the line to run in the survivor, so the side effect after
// the `sleep` still happened.
func TestCancelStopsTheRestOfTheLineAndResetsTheSession(t *testing.T) {
	session := newTestSession(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	sideEffect := filepath.Join(dir, "after-cancel")

	var before strings.Builder
	if code, err := session.Exec("cd "+dir+"; export WANCTL_MARK=set", &before); err != nil || code != 0 {
		t.Fatalf("session setup = %d %v", code, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runAsync(session, ctx, parkChild(pidFile, 600)+"; echo AFTER_CANCEL > "+sideEffect+"; cd /; export WANCTL_MARK=changed")
	pid := waitForPIDFile(t, pidFile)
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })

	start := time.Now()
	cancel()
	r := awaitExec(t, done, 10*time.Second)

	if !errors.Is(r.err, ErrSessionCancelled) {
		t.Fatalf("cancelled command returned (%d, %v), want ErrSessionCancelled", r.code, r.err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the cancel took %s to come back", took)
	}
	if !processGone(pid, 3*time.Second) {
		t.Fatalf("device-side process %d survived the cancel", pid)
	}
	if _, err := os.Stat(sideEffect); err == nil {
		t.Fatal("the statement after the cancelled one still ran and wrote its file")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if !session.Closed() {
		t.Fatal("the session is still open; a cancelled session is destroyed on purpose")
	}
	if _, err := session.Exec("echo late", io.Discard); err == nil {
		t.Fatal("a destroyed session still accepted a command")
	}
}

// The regression for the review's finding: a cancellation armed by one request
// must never land on the next one. Here the first request's cancel is parked
// inside the kill while that request finishes normally, which is exactly the
// interleaving that used to kill an uncancelled command with exit 137.
func TestCancelFromAnEarlierRequestCannotReachTheNext(t *testing.T) {
	session := newTestSession(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var kills atomic.Int32
	session.gate = newCancelGate(func() error {
		close(entered)
		<-release
		kills.Add(1)
		return session.container.Kill()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := runAsync(session, ctx, "/bin/sleep 1")

	time.Sleep(200 * time.Millisecond) // the command is running, the gate is armed
	cancel()
	<-entered // the cancel is inside the kill and cannot proceed

	// The first command finishes on its own (the sleep is only a second) and
	// then tries to disarm. It must block until the cancellation it armed has
	// resolved, or that cancellation would still be in flight when the next
	// command starts.
	select {
	case r := <-first:
		t.Fatalf("the request returned (%d, %v) while its own cancellation was still running", r.code, r.err)
	case <-time.After(2 * time.Second):
	}

	close(release)
	r1 := awaitExec(t, first, 10*time.Second)
	if !errors.Is(r1.err, ErrSessionCancelled) {
		t.Fatalf("the cancelled request returned (%d, %v), want ErrSessionCancelled", r1.code, r1.err)
	}

	// The next command must not be quietly killed by that cancellation. The
	// session is gone, so it is refused outright.
	victim := filepath.Join(t.TempDir(), "second-ran")
	code, err := session.ExecInDirContext(context.Background(), "echo second > "+victim, "", io.Discard)
	if err == nil {
		t.Fatalf("the next command ran on a destroyed session and returned %d", code)
	}
	if _, statErr := os.Stat(victim); statErr == nil {
		t.Fatal("the next command ran; it should have been refused, not executed and killed")
	}
	if got := kills.Load(); got != 1 {
		t.Fatalf("the kill ran %d times, want exactly once", got)
	}
}

// The mirror case: a cancellation that wakes after its request has already
// disarmed must do nothing at all.
func TestCancelThatWakesAfterItsRequestIsANoOp(t *testing.T) {
	session := newTestSession(t)
	var kills atomic.Int32
	session.gate = newCancelGate(func() error {
		kills.Add(1)
		return session.container.Kill()
	})

	ctx, cancel := context.WithCancel(context.Background())
	var out strings.Builder
	code, err := session.ExecInDirContext(ctx, "echo first-finished", "", &out)
	if err != nil || code != 0 || !strings.Contains(out.String(), "first-finished") {
		t.Fatalf("first command = %d %v out=%q", code, err, out.String())
	}
	cancel() // the request is over; this must not touch the session
	time.Sleep(200 * time.Millisecond)

	out.Reset()
	code, err = session.ExecInDirContext(context.Background(), "echo second-finished", "", &out)
	if err != nil || code != 0 || !strings.Contains(out.String(), "second-finished") {
		t.Fatalf("second command = %d %v out=%q", code, err, out.String())
	}
	if got := kills.Load(); got != 0 {
		t.Fatalf("a cancellation belonging to a finished request fired %d times", got)
	}
	if session.Closed() {
		t.Fatal("the session was destroyed by a cancellation that belonged to nothing")
	}
}

// A request cancelled before it reached the shell — the controller left while
// it sat behind another command — must not run at all.
func TestPreCancelledRequestRunsNothing(t *testing.T) {
	session := newTestSession(t)
	var kills atomic.Int32
	session.gate = newCancelGate(func() error { kills.Add(1); return nil })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	effect := filepath.Join(t.TempDir(), "should-not-exist")
	code, err := session.ExecInDirContext(ctx, "echo ran > "+effect, "", io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled request = %d %v, want context.Canceled", code, err)
	}
	if _, statErr := os.Stat(effect); statErr == nil {
		t.Fatal("a pre-cancelled request still executed its command")
	}
	if got := kills.Load(); got != 0 {
		t.Fatalf("a pre-cancelled request killed the session %d times; it should never have started", got)
	}
	if session.Closed() {
		t.Fatal("a pre-cancelled request destroyed a session it never used")
	}
}

// `set -e` used to make a cancel fatal in a way the caller could not see: the
// shell exited on the child's non-zero status and never printed a marker. With
// the session destroyed by design there is nothing left to get wrong, but the
// case is worth pinning.
func TestCancelUnderErrexitReportsCancellation(t *testing.T) {
	session := newTestSession(t)
	if _, err := session.Exec("set -e", io.Discard); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runAsync(session, ctx, parkChild(pidFile, 600))
	pid := waitForPIDFile(t, pidFile)
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })

	cancel()
	r := awaitExec(t, done, 10*time.Second)
	if !errors.Is(r.err, ErrSessionCancelled) {
		t.Fatalf("cancel under errexit = (%d, %v), want ErrSessionCancelled", r.code, r.err)
	}
	if !processGone(pid, 3*time.Second) {
		t.Fatalf("device-side process %d survived a cancel under errexit", pid)
	}
}

// A kill that failed must reach the caller. Reporting a clean cancellation when
// the device could not stop anything is the one answer that is never true.
func TestFailedKillIsReportedNotSwallowed(t *testing.T) {
	session := newTestSession(t)
	session.gate = newCancelGate(func() error { return errors.New("no permission to signal the group") })

	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runAsync(session, ctx, parkChild(pidFile, 2))
	waitForPIDFile(t, pidFile)
	cancel()

	r := awaitExec(t, done, 10*time.Second)
	if !errors.Is(r.err, ErrSessionCancelled) {
		t.Fatalf("failed cancel = (%d, %v), want it to still report a cancellation", r.code, r.err)
	}
	if !strings.Contains(r.err.Error(), "no permission to signal the group") {
		t.Fatalf("failed cancel reported %q without saying the kill failed", r.err)
	}
}
