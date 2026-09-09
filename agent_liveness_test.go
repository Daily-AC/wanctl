package main

import (
	"os"
	"os/exec"
	"testing"

	"wanctl/internal/config"
)

// The reported failure: a controller-only Windows PC ran `wanctl update` and
// came back registered as a controlled device. A stale agent.pid plus pid reuse
// made the update believe an agent was running, so it "restarted" one that had
// never existed. Liveness comes from the lock now, so a live pid alone plans
// nothing.
func TestPlanUpdateRestartIgnoresALivePIDWithNoLock(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	if err := config.WritePID(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if plan := planUpdateRestart(false); plan != (updateRestartPlan{}) {
		t.Fatalf("a live pid with a free lock planned %+v; nothing may be stopped or started", plan)
	}
	// The rule this replaced, kept here so the test keeps proving something: read
	// liveness off the pid number and the same config dir plans a stop and a
	// start, which is the reported bug exactly.
	if plan := planUpdateRestartWithLiveness(false, os.Getpid(), processAlive(os.Getpid())); !plan.stopDetached || !plan.restartDetached {
		t.Fatal("the pid-based rule no longer reproduces the bug; this test has stopped guarding it")
	}
}

// With an agent actually holding the lock, the same update owes it a restart.
func TestPlanUpdateRestartRestartsAnAgentHoldingTheLock(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	lock, err := config.AcquireAgentLock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := config.WritePID(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	plan := planUpdateRestart(false)
	if !plan.stopDetached || !plan.restartDetached || plan.restartManagedPID != 0 {
		t.Fatalf("plan = %+v, want a detached stop and restart", plan)
	}
	if plan := planUpdateRestart(true); plan != (updateRestartPlan{}) {
		t.Fatalf("--no-restart planned %+v", plan)
	}
}

// agentRunning clears a pid file whose process is gone, and leaves alone one
// whose process is alive: that second file is either pid reuse or an agent that
// was launched a moment ago and has not reached AcquireAgentLock yet, and
// deleting it would hide a starting agent from `wanctl stop` permanently.
func TestAgentRunningClearsOnlyProvablyStalePIDFiles(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())

	// A pid that cannot be running: nothing owns it, so the file goes.
	if err := config.WritePID(deadPID(t)); err != nil {
		t.Fatal(err)
	}
	if pid, running := agentRunning(); running {
		t.Fatalf("a dead pid reported running (pid %d)", pid)
	}
	if pid := config.ReadPID(); pid != 0 {
		t.Fatalf("a provably stale pid file survived: %d", pid)
	}

	// A live pid with a free lock: not running, but the file stays.
	if err := config.WritePID(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if _, running := agentRunning(); running {
		t.Fatal("a live pid with a free lock reported running")
	}
	if pid := config.ReadPID(); pid != os.Getpid() {
		t.Fatalf("a possibly-starting agent's pid file was deleted (pid %d)", pid)
	}
}

// cmdStop must never signal a pid whose lock nobody holds. The assertion is the
// test surviving: the pid file names this test process, so a stop that signalled
// it would terminate the test binary rather than fail an assertion.
func TestStopRefusesAPIDThatHoldsNoLock(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	if err := config.WritePID(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := cmdStop(); err != nil {
		t.Fatalf("stop with a stale pid file = %v, want a clean no-op", err)
	}
	if pid := config.ReadPID(); pid != os.Getpid() {
		t.Fatalf("stop deleted the pid file of a process it could not account for: %d", pid)
	}
}

// The other half: with nothing recorded at all, stop is a quiet no-op.
func TestStopWithNoPIDFileIsANoOp(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	if err := cmdStop(); err != nil {
		t.Fatalf("stop on an idle config dir = %v", err)
	}
}

// deadPID returns a pid that named a real process and no longer does, so the
// test does not depend on some hard-coded number happening to be free. Re-running
// this test binary with a filter that matches nothing is the most portable way
// to get a process that starts and exits at once.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=NoTestMatchesThisPattern")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn a helper process: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()
	if processAlive(pid) {
		t.Skipf("pid %d is still alive after the helper exited", pid)
	}
	return pid
}
