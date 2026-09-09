package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"wanctl/internal/config"
)

// The incident: `wanctl update` stopped the agent, swapped the binary and
// started a new one while the old process was still shutting down and still
// holding the config-dir lock. The new agent could not acquire it and exited,
// the parent had already reported success, and the device was off the relay for
// fifty minutes. Stopping now means "gone", not "told to go".
func TestStopWaitsForTheAgentToReleaseTheLock(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	lock, err := config.AcquireAgentLock()
	if err != nil {
		t.Fatal(err)
	}
	if err := config.WritePID(999999); err != nil {
		t.Fatal(err)
	}

	released := make(chan struct{})
	restore := terminateAgent
	terminateAgent = func(pid int) error {
		if pid != 999999 {
			t.Errorf("signalled pid %d, want the recorded one", pid)
		}
		go func() {
			time.Sleep(300 * time.Millisecond)
			lock.Close()
			close(released)
		}()
		return nil
	}
	defer func() { terminateAgent = restore }()

	start := time.Now()
	if err := cmdStop(); err != nil {
		t.Fatalf("stop = %v, want it to wait and succeed", err)
	}
	if waited := time.Since(start); waited < 300*time.Millisecond {
		t.Fatalf("stop returned after %s, before the agent let go", waited)
	}
	<-released
	// Nothing holds the lock, so the start that follows cannot race a dying
	// predecessor -- which is the whole point of waiting.
	if pid, running := config.AgentRunning(); running {
		t.Fatalf("the lock is still held after stop (pid %d)", pid)
	}
}

// An agent that will not let go must be reported, not worked around: replacing
// the binary and starting a second agent behind it is how the device ended up
// with none.
func TestStopFailsLoudlyWhenTheAgentNeverReleases(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	lock, err := config.AcquireAgentLock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := config.WritePID(999999); err != nil {
		t.Fatal(err)
	}

	restoreTerm, restoreTimeout, restorePoll := terminateAgent, agentStopTimeout, agentStopPoll
	terminateAgent = func(int) error { return nil }
	agentStopTimeout, agentStopPoll = 150*time.Millisecond, 10*time.Millisecond
	defer func() {
		terminateAgent, agentStopTimeout, agentStopPoll = restoreTerm, restoreTimeout, restorePoll
	}()

	err = cmdStop()
	if err == nil {
		t.Fatal("stop reported success while the agent still held the lock")
	}
	if !strings.Contains(err.Error(), "999999") || !strings.Contains(err.Error(), "wanctl start") {
		t.Fatalf("stop error = %q; it must name the pid and what to do next", err)
	}
	// The pid file is left alone: the agent it names is still running.
	if pid := config.ReadPID(); pid != 999999 {
		t.Fatalf("a failed stop cleared the pid file (%d)", pid)
	}
}

// `wanctl start` must never spawn a second agent into a config dir that already
// has one.
func TestStartRefusesWhileTheLockIsHeld(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_TOKEN", "tok")
	lock, err := config.AcquireAgentLock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := config.WritePID(999999); err != nil {
		t.Fatal(err)
	}
	if err := cmdStart(context.Background()); err != nil {
		t.Fatalf("start with a held lock = %v", err)
	}
	if pid := config.ReadPID(); pid != 999999 {
		t.Fatalf("start replaced the running agent's pid with %d", pid)
	}
}

func TestAwaitAgentLockReleaseDecisionTable(t *testing.T) {
	held := 3
	slept := 0
	running := func() (int, bool) {
		if held > 0 {
			held--
			return 7, true
		}
		return 0, false
	}
	if !awaitAgentLockRelease(running, time.Second, 10*time.Millisecond, func(time.Duration) { slept++ }) {
		t.Fatal("a lock released on the fourth probe was reported as stuck")
	}
	if slept != 3 {
		t.Fatalf("slept %d times, want one per busy probe", slept)
	}
	stuck := func() (int, bool) { return 7, true }
	if awaitAgentLockRelease(stuck, 30*time.Millisecond, 10*time.Millisecond, func(time.Duration) {}) {
		t.Fatal("a lock that is never released was reported as free")
	}
}

// The agent side of the same race: it can be started while its predecessor is
// still shutting down, and the parent has already told the user it is running.
func TestAwaitAgentLockRetriesOnlyWhileHeld(t *testing.T) {
	heldErr := lockHeldError(t)
	calls := 0
	acquire := func() (*config.AgentLock, error) {
		calls++
		if calls < 3 {
			return nil, heldErr
		}
		return nil, nil
	}
	if _, err := awaitAgentLock(acquire, 10, time.Millisecond, func(time.Duration) {}); err != nil {
		t.Fatalf("retry gave up on a lock that freed up: %v", err)
	}
	if calls != 3 {
		t.Fatalf("acquired %d times, want a retry per held attempt", calls)
	}

	calls = 0
	if _, err := awaitAgentLock(func() (*config.AgentLock, error) { calls++; return nil, heldErr }, 4, time.Millisecond, func(time.Duration) {}); !config.IsAgentLockHeld(err) {
		t.Fatalf("error after exhausting retries = %v", err)
	}
	if calls != 5 {
		t.Fatalf("attempted %d times, want the first try plus four retries", calls)
	}

	// Anything that is not "someone else has it" is returned at once.
	other := errors.New("permission denied")
	calls = 0
	if _, err := awaitAgentLock(func() (*config.AgentLock, error) { calls++; return nil, other }, 10, time.Millisecond, func(time.Duration) {}); !errors.Is(err, other) {
		t.Fatalf("error = %v, want it passed through", err)
	}
	if calls != 1 {
		t.Fatalf("retried a non-lock error %d times", calls)
	}
}

// The message an agent prints when it loses the race must not send the reader
// after a process that is the one printing it.
func TestLockHeldMessageDoesNotBlameItself(t *testing.T) {
	self := os.Getpid()
	own := lockHeldMessage(self, self)
	if strings.Contains(own, "another agent") {
		t.Fatalf("an agent blamed itself: %q", own)
	}
	if !strings.Contains(own, "wanctl start") {
		t.Fatalf("the message must say what to do next: %q", own)
	}
	if none := lockHeldMessage(0, self); strings.Contains(none, "another agent") {
		t.Fatalf("an unrecorded pid was reported as another agent: %q", none)
	}
	other := lockHeldMessage(4242, self)
	if !strings.Contains(other, "another agent") || !strings.Contains(other, "4242") {
		t.Fatalf("a genuinely different holder must be named: %q", other)
	}
}

// lockHeldError produces the real "someone else holds it" error, so the retry
// test is driven by the same value the agent will actually see.
func lockHeldError(t *testing.T) error {
	t.Helper()
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	held, err := config.AcquireAgentLock()
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	second, err := config.AcquireAgentLock()
	if err == nil {
		second.Close()
		t.Fatal("a second lock succeeded")
	}
	return err
}
