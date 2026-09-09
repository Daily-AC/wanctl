package config

import (
	"os"
	"testing"
)

func TestAcquireAgentLockExcludesSameConfigDirAndReleases(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())

	first, err := AcquireAgentLock()
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	second, err := AcquireAgentLock()
	if err == nil {
		second.Close()
		t.Fatal("second lock unexpectedly succeeded")
	}
	if !IsAgentLockHeld(err) {
		t.Fatalf("second lock error = %v, want held", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first lock: %v", err)
	}
	third, err := AcquireAgentLock()
	if err != nil {
		t.Fatalf("third lock after release: %v", err)
	}
	third.Close()
}

// AgentRunning must answer from the lock, never from the number in agent.pid.
// The number is a label: it outlives the agent that wrote it, and the operating
// system reissues it to something unrelated.
func TestAgentRunningReadsTheLockNotThePIDFile(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())

	// No pid file, no lock: nothing is running.
	if pid, running := AgentRunning(); running || pid != 0 {
		t.Fatalf("empty config dir = pid %d running %v", pid, running)
	}

	// A pid file naming this very process -- as alive as a pid can be -- while
	// no agent holds the lock. This is the stale-pid-plus-reuse case that made
	// `wanctl update` restart an agent that had never run.
	if err := WritePID(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	pid, running := AgentRunning()
	if running {
		t.Fatal("a live pid with a free lock was reported as a running agent")
	}
	if pid != os.Getpid() {
		t.Fatalf("recorded pid = %d, want %d", pid, os.Getpid())
	}

	// The lock held is what running means, and the probe must not disturb it.
	lock, err := AcquireAgentLock()
	if err != nil {
		t.Fatal(err)
	}
	if pid, running := AgentRunning(); !running || pid != os.Getpid() {
		t.Fatalf("held lock = pid %d running %v", pid, running)
	}
	if pid, running := AgentRunning(); !running || pid != os.Getpid() {
		t.Fatalf("second probe changed the answer: pid %d running %v", pid, running)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("the probe broke the holder's lock: %v", err)
	}
	if _, running := AgentRunning(); running {
		t.Fatal("still running after the holder released")
	}
}

// An agent that holds the lock without having recorded a pid is running, and
// callers are told so even though they have nothing to signal.
func TestAgentRunningReportsAHeldLockWithNoPIDFile(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	lock, err := AcquireAgentLock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if pid, running := AgentRunning(); !running || pid != 0 {
		t.Fatalf("held lock with no pid file = pid %d running %v", pid, running)
	}
}
