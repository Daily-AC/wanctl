//go:build !windows

package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// A kill that failed must reach the caller, and reach it while the command it
// could not stop is still running. Waiting for that command to end by itself is
// the one thing a cancellation cannot do.
func TestFailedKillIsReportedWhileTheCommandStillRuns(t *testing.T) {
	session := newTestSession(t)
	// The kill does nothing and reports why. The shell therefore survives and
	// the marker never comes, so nothing but cutting the output can return
	// this command — which is the point. The session's own Close cleans up.
	session.killContainer = func() error {
		return errors.New("no permission to signal the group")
	}

	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runAsync(session, ctx, parkChild(pidFile, 600))
	pid := waitForPIDFile(t, pidFile)
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })

	start := time.Now()
	cancel()
	r := awaitExec(t, done, 5*time.Second)
	// Comfortably inside sessionWaitDelay: this must not be the reaper timing
	// out on the pipes, it must be the cancellation cutting the output itself.
	if took := time.Since(start); took > sessionWaitDelay/2 {
		t.Fatalf("the failed kill took %s to reach the caller; the command runs for ten minutes", took)
	}
	if !errors.Is(r.err, ErrSessionCancelled) {
		t.Fatalf("failed cancel = (%d, %v), want it to still report a cancellation", r.code, r.err)
	}
	if !strings.Contains(r.err.Error(), "no permission to signal the group") {
		t.Fatalf("failed cancel reported %q without saying the kill failed", r.err)
	}
}

// A watcher can wake after its own request disarmed and after the next request
// armed. A gate that only asks "is anything armed" reads true in that state and
// destroys a session nobody cancelled — 50 times in 100 under the scheduling
// the review used. Generations make the late watcher name a request that is
// over.
func TestLateWatcherCannotFireForTheRequestThatFollowedIt(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	for range 100 {
		var kills atomic.Int32
		gate := newCancelGate(func() error { kills.Add(1); return nil })

		first, cancelFirst := context.WithCancel(context.Background())
		disarmFirst := gate.arm(first)
		disarmFirst()
		cancelFirst() // the first request is over; its watcher wakes now

		second, cancelSecond := context.WithCancel(context.Background())
		disarmSecond := gate.arm(second)
		runtime.Gosched() // let the first watcher run against the second arming
		fired, _ := disarmSecond()
		cancelSecond()
		runtime.Gosched()

		if fired {
			t.Fatalf("a request whose context was never cancelled was reported cancelled")
		}
		if got := kills.Load(); got != 0 {
			t.Fatalf("a watcher belonging to a finished request killed the session %d times", got)
		}
	}
}

// The same thing against a real shell, with the stale callback invoked directly
// so there is no scheduling to wait on.
func TestStaleCancelCallbackCannotKillTheNextCommand(t *testing.T) {
	session := newTestSession(t)
	var stale func()
	first, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	// Capture the firing callback exactly as the first request's watcher holds
	// it, generation and all, then end that request.
	gen := armAndCaptureGeneration(t, session, first)
	stale = func() { session.gate.fire(gen) }

	second, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	done := runAsync(session, second, parkChild(pidFile, 600))
	pid := waitForPIDFile(t, pidFile)
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })

	stale() // the first request's cancellation, arriving during the second

	select {
	case r := <-done:
		t.Fatalf("an uncancelled command was stopped by an earlier request's cancellation: (%d, %v)", r.code, r.err)
	case <-time.After(500 * time.Millisecond):
	}
	if session.Closed() {
		t.Fatal("the session was destroyed by a cancellation belonging to a finished request")
	}
	if second.Err() != nil {
		t.Fatal("the second request's own context was cancelled; the test proves nothing")
	}

	cancelSecond()
	if r := awaitExec(t, done, 5*time.Second); !errors.Is(r.err, ErrSessionCancelled) {
		t.Fatalf("the second request's own cancel = (%d, %v)", r.code, r.err)
	}
}

// armAndCaptureGeneration arms the gate for ctx, ends that request, and returns
// the generation its watcher would fire with.
func armAndCaptureGeneration(t *testing.T, session *ShellSession, ctx context.Context) uint64 {
	t.Helper()
	disarm := session.gate.arm(ctx)
	session.gate.mu.Lock()
	gen := session.gate.next
	session.gate.mu.Unlock()
	disarm()
	return gen
}

// A descendant that left the container still holds the shell's stdout. The
// cancellation must not wait for it: if it does, the command never returns,
// Closed() never answers, and the session can never be replaced.
func TestEscapedDescendantCannotHoldACancelledSession(t *testing.T) {
	session := newTestSession(t)
	pidFile := filepath.Join(t.TempDir(), "escaped.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// `set -m` turns on job control, which puts the background job in a process
	// group of its own — outside the container the session is killed by.
	done := runAsync(session, ctx, "set -m; /bin/sleep 600 & echo $! > "+pidFile+"; wait")
	pid := waitForPIDFile(t, pidFile)
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })

	shellGroup, err := syscall.Getpgid(session.cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	childGroup, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatal(err)
	}
	if childGroup == shellGroup {
		t.Skipf("this shell keeps job-control children in the session's group (%d); nothing escapes to test", shellGroup)
	}

	start := time.Now()
	cancel()
	r := awaitExec(t, done, 5*time.Second)
	// Inside sessionWaitDelay on purpose. The reaper eventually closes the
	// pipes anyway, so a looser bound would pass without the cancellation
	// cutting the output at all.
	if took := time.Since(start); took > sessionWaitDelay/2 {
		t.Fatalf("the cancel waited %s for a process that had left the container", took)
	}
	if !errors.Is(r.err, ErrSessionCancelled) {
		t.Fatalf("cancel with an escaped descendant = (%d, %v)", r.code, r.err)
	}

	// Closed() must answer immediately: the agent asks it while holding the
	// lock that guards every session on the device.
	answered := make(chan bool, 1)
	go func() { answered <- session.Closed() }()
	select {
	case closed := <-answered:
		if !closed {
			t.Fatal("the session reports itself open after being cancelled")
		}
	case <-time.After(time.Second):
		t.Fatal("Closed() blocked; every other session on the device would be stalled behind it")
	}

	if syscall.Kill(pid, 0) != nil {
		t.Log("the escaped process died anyway on this shell")
	}
}

// A container is signalled at most once and never after the shell is reaped,
// because a process-group id is only a number and can be given to another group
// once this one is gone.
func TestContainerKillsOnceAndNeverAfterReap(t *testing.T) {
	t.Run("only the first kill signals", func(t *testing.T) {
		cmd := exec.Command("/bin/sh", "-c", "sleep 30")
		prepareSessionContainer(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		container, err := captureSessionContainer(cmd)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		if err := container.Kill(); err != nil {
			t.Fatalf("first kill = %v", err)
		}
		pgid := container.pgid
		if err := container.Kill(); err != nil {
			t.Fatalf("second kill = %v, want a no-op", err)
		}
		if container.pgid != pgid {
			t.Fatal("the second kill changed the container's state")
		}
		if !container.killed {
			t.Fatal("the container does not remember that it killed")
		}
	})

	t.Run("a reaped container stops naming its group", func(t *testing.T) {
		container := &sessionContainer{pgid: 424242}
		container.reap()
		if container.pgid != 0 {
			t.Fatalf("a reaped container still holds pgid %d, which may belong to someone else now", container.pgid)
		}
		if err := container.Kill(); err != nil {
			t.Fatalf("kill after reap = %v, want a no-op", err)
		}
		if container.killed {
			t.Fatal("kill after reap signalled a group id that no longer names this session")
		}
	})
}

// A background process left in the group must not survive a natural shell
// exit. The probe from issue #111: `sleep 600 & …; exit` — the shell is
// waited, Close returns, and without a kill-before-reap the sleep keeps
// running with PGID == the old shell pid. It never called setsid/setpgid,
// so it is not in the documented escape list.
func TestCloseKillsBackgroundProcessAfterNaturalShellExit(t *testing.T) {
	session, err := NewShellSession("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "bg.pid")
	_, _ = session.Exec("sleep 600 </dev/null >/dev/null 2>&1 & echo $! > "+pidFile+"; exit", io.Discard)
	pid := waitForPIDFile(t, pidFile)
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })

	session.Close()
	if !processGone(pid, 3*time.Second) {
		t.Fatalf("background process %d survived Close after a natural shell exit", pid)
	}
}

// cmd.Wait returns only after I/O goroutines finish (or WaitDelay elapses).
// The kernel has already reaped the shell by then, so the pid — and the
// process-group id that is the same number — may already belong to someone
// else. The container must drop that number at the kernel wait, not when
// Wait returns (issue #110).
func TestReapHappensAtKernelWaitNotAfterIO(t *testing.T) {
	session := newTestSession(t)
	pid := session.cmd.Process.Pid

	done := runAsync(session, context.Background(), "exit")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatal("the shell never exited")
	}

	// Comfortably inside sessionWaitDelay: if reap were tied to cmd.Wait
	// returning, the container would still name the old pgid here.
	seen := time.Now()
	for time.Now().Before(seen.Add(sessionWaitDelay / 4)) {
		session.container.mu.Lock()
		reaped := session.container.reaped
		pgid := session.container.pgid
		wasKilled := session.container.killed
		session.container.mu.Unlock()
		if reaped && pgid == 0 {
			if err := session.container.Kill(); err != nil {
				t.Fatalf("kill after kernel reap = %v, want a no-op", err)
			}
			session.container.mu.Lock()
			pgidAfter := session.container.pgid
			killedAfter := session.container.killed
			session.container.mu.Unlock()
			if pgidAfter != 0 {
				t.Fatal("kill after kernel reap restored a group id that no longer names this session")
			}
			if killedAfter != wasKilled {
				t.Fatal("kill after kernel reap signalled a group id that no longer names this session")
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Log("the exiting command's Exec did not return; the session is still closed by Cleanup")
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	session.container.mu.Lock()
	reaped := session.container.reaped
	pgid := session.container.pgid
	session.container.mu.Unlock()
	t.Fatalf("container still names pgid %d reaped=%v after the kernel reaped pid %d; Kill could hit a stranger", pgid, reaped, pid)
}

// A shell that could not be contained must be killed, reaped and have both ends
// of its output pipe closed. Leaving the copier blocked writing into a pipe
// nobody reads leaks a goroutine and two descriptors per attempt.
func TestShellThatCannotBeContainedIsCleanedUp(t *testing.T) {
	wrapper := filepath.Join(t.TempDir(), "noisy-shell")
	script := "#!/bin/sh\necho startup\nwhile :; do sleep 1; done\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	injected := errors.New("injected containment failure")
	restore := captureSession
	captureSession = func(cmd *exec.Cmd) (*sessionContainer, error) { return nil, injected }
	t.Cleanup(func() { captureSession = restore })

	settle := func() {
		for range 40 {
			runtime.GC()
			time.Sleep(25 * time.Millisecond)
		}
	}
	settle()
	before := runtime.NumGoroutine()
	for range 3 {
		if _, err := NewShellSession(wrapper); !errors.Is(err, injected) {
			t.Fatalf("NewShellSession = %v, want the injected failure", err)
		}
	}
	settle()
	if after := runtime.NumGoroutine(); after > before+1 {
		t.Fatalf("three failed starts left %d goroutines behind (%d -> %d)", after-before, before, after)
	}
}
