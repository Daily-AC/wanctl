package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"wanctl/internal/config"
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

// A controller-only host — wanctl installed, logged in, never started as an
// agent — must come out of an update exactly as it went in. GitHub issue #66:
// on such a Windows machine `wanctl update` started an agent, which enrolled
// the machine, which put it in the owner's device list as a controlled device.
//
// The assertion is the whole config directory, not just the plan: an update
// that enrolls leaves a token behind, and one that starts an agent leaves a pid
// file and a lock. Both halves of the plan are executed here, so a future
// change that makes either unconditional fails this.
func TestUpdateLeavesAControllerOnlyHostAlone(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WANCTL_CONFIG_DIR", dir)
	// Logged in for controller use, which is the reported machine: credentials
	// are not an agent, and must not be read as one.
	if err := config.SaveToken("controller-token"); err != nil {
		t.Fatal(err)
	}
	before := configDirEntries(t, dir)

	plan := planUpdateRestart(false)
	if plan != (updateRestartPlan{}) {
		t.Fatalf("plan = %+v; a host with no agent is owed nothing", plan)
	}
	if err := applyUpdateStop(plan); err != nil {
		t.Fatalf("stop half: %v", err)
	}
	if err := applyUpdateRestart(t.Context(), filepath.Join(dir, "wanctl"), plan); err != nil {
		t.Fatalf("restart half: %v", err)
	}

	if pid := config.ReadPID(); pid != 0 {
		t.Errorf("an agent was started: pid file names %d", pid)
	}
	if got := configDirEntries(t, dir); !slices.Equal(got, before) {
		t.Errorf("config dir went from %v to %v; the update must not write here", before, got)
	}
	if tok := config.StoredToken(); tok != "controller-token" {
		t.Errorf("the controller's own credentials changed to %q — a re-enrollment", tok)
	}
}

// The same rule reached through the other entry point's shape: the sudo-split
// update reads the plan with noRestart hardcoded false, because it is the user
// phase and the root phase is the one that passes --no-restart. A controller-
// only host must still get the empty plan there.
func TestSudoSplitUpdateAlsoPlansNothingWithoutAnAgent(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	if plan := planUpdateRestart(false); plan != (updateRestartPlan{}) {
		t.Fatalf("plan = %+v, want nothing", plan)
	}
}

// configDirEntries is the config directory's contents by name, so a test can
// say "unchanged" about a directory rather than about the three files it
// remembered to check.
func configDirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

// The corner the pid check exists for: an agent holds the lock but recorded no
// pid. It is running, so the update owes it a restart, and the plan has to say
// something a caller can act on. It used to collapse into "restart supervised
// agent 0" — the two zeros of an unrecorded pid and an absent supervisor marker
// comparing equal — which reads as a plan and behaves as nothing.
func TestPlanUpdateRestartDoesNotMistakeAnUnrecordedPIDForASupervisedAgent(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	lock, err := config.AcquireAgentLock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	// No WritePID: the agent locked but never recorded itself.
	plan := planUpdateRestart(false)
	if plan == (updateRestartPlan{}) || plan.restartManagedPID != 0 {
		t.Fatalf("plan = %+v; a running agent with no recorded pid is owed an actionable restart", plan)
	}
	if !plan.stopDetached || !plan.restartDetached {
		t.Fatalf("plan = %+v, want a detached stop and restart", plan)
	}
}
