package main

import (
	"os"
	"testing"
	"time"
)

// TestAwaitManagedRestartOutcomes pins the three states an upgrade can end in.
// Only the first is success; the other two used to be reported as success too,
// which is how a device kept serving the old build after `wanctl update` said
// "✓ 已安装".
func TestAwaitManagedRestartOutcomes(t *testing.T) {
	const old = 4242

	t.Run("supervisor puts a new agent in place", func(t *testing.T) {
		polls := 0
		alive := func(pid int) bool { return pid == old || pid == 5150 }
		current := func() int {
			polls++
			if polls < 3 {
				return old
			}
			return 5150
		}
		if got := awaitManagedRestart(old, alive, current, 10, 0, func(time.Duration) {}); got != managedRestartReplaced {
			t.Fatalf("result = %v, want replaced", got)
		}
	})

	t.Run("old agent never exits", func(t *testing.T) {
		slept := 0
		got := awaitManagedRestart(old,
			func(int) bool { return true },
			func() int { return old },
			4, time.Second,
			func(time.Duration) { slept++ })
		if got != managedRestartStuck {
			t.Fatalf("result = %v, want stuck", got)
		}
		if slept != 4 {
			t.Fatalf("slept %d times, want 4 (must give up, not spin forever)", slept)
		}
	})

	t.Run("old agent exits and nothing takes over", func(t *testing.T) {
		got := awaitManagedRestart(old,
			func(int) bool { return false },
			func() int { return 0 },
			3, 0, func(time.Duration) {})
		if got != managedRestartStopped {
			t.Fatalf("result = %v, want stopped", got)
		}
	})

	// A pid file left behind by a supervisor that failed to restart names a
	// process that is not running. That is not a successful upgrade.
	t.Run("registered pid is dead", func(t *testing.T) {
		got := awaitManagedRestart(old,
			func(pid int) bool { return pid == old },
			func() int { return 5150 },
			2, 0, func(time.Duration) {})
		if got != managedRestartStuck {
			t.Fatalf("result = %v, want stuck", got)
		}
	})
}

// TestForeignProcessIsAliveButNotOurs covers the probe pair against a real
// process this user cannot signal. pid 1 is owned by root on every supported
// Unix, so an agent under a root-owned supervisor must read as running (or
// `wanctl status` lies and `wanctl update` skips the restart it owes) while
// still being off limits to terminate.
func TestForeignProcessIsAliveButNotOurs(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: every process is ours to signal")
	}
	if !processAlive(1) {
		t.Error("processAlive(1) = false; a process owned by another user is still running")
	}
	if canTerminatePID(1) {
		t.Error("canTerminatePID(1) = true; this user cannot signal a root-owned process")
	}
	if canTerminatePID(os.Getpid()) != true {
		t.Error("canTerminatePID(self) = false; want true")
	}
	for _, pid := range []int{0, -1} {
		if processAlive(pid) || canTerminatePID(pid) {
			t.Errorf("pid %d reported usable", pid)
		}
	}
}
