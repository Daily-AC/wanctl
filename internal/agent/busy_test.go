package agent

import (
	"io"
	"runtime"
	"testing"
	"time"

	"wanctl/internal/server"
)

// TestBusyCoversEveryKindOfWork pins the gate the auto-updater consults before
// replacing the binary under a live agent. Getting it wrong in the permissive
// direction kills a controller's shell session mid-command; getting it wrong in
// the restrictive direction leaves a device on an old build forever, because
// something is always nearly-busy on a machine people use.
func TestBusyCoversEveryKindOfWork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh for the job half")
	}
	a := &Agent{sessions: map[string]*server.ShellSession{}, jobs: newJobStore()}
	if a.Busy() {
		t.Fatal("a fresh agent with nothing running reads as busy")
	}

	t.Run("a shell session runs a command", func(t *testing.T) {
		sess, err := server.NewShellSession("/bin/sh")
		if err != nil {
			t.Fatal(err)
		}
		a.sessions["SHA256:controller"] = sess
		if a.Busy() {
			t.Fatal("an idle shell session counts as busy")
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			sess.Exec("sleep 1", io.Discard)
		}()
		deadline := time.Now().Add(time.Second)
		for !a.Busy() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if !a.Busy() {
			t.Fatal("a command running in a shell session does not count as busy")
		}
		<-done
		sess.Close()
		// A closed session is history: its cwd and environment are already
		// gone, so nothing is lost by restarting around it.
		if a.Busy() {
			t.Fatal("a closed session still counts as busy")
		}
	})

	t.Run("a running background job", func(t *testing.T) {
		if _, err := a.jobs.start("controller", "/bin/sh", "sleep 5", ""); err != nil {
			t.Fatal(err)
		}
		if !a.Busy() {
			t.Fatal("a running async job does not count as busy")
		}
	})

	t.Run("a live console session", func(t *testing.T) {
		b := &Agent{sessions: map[string]*server.ShellSession{}, jobs: newJobStore()}
		b.consoles.Add(1)
		if !b.Busy() {
			t.Fatal("a live console session does not count as busy")
		}
		b.consoles.Add(-1)
		if b.Busy() {
			t.Fatal("busy after the console session ended")
		}
	})

	// An agent assembled by a unit test without New has no job store; asking it
	// whether it is busy must answer, not panic.
	t.Run("an agent with no job store", func(t *testing.T) {
		if (&Agent{}).Busy() {
			t.Fatal("an empty agent reads as busy")
		}
	})
}

// One plain exec leaves its shell session open for the next one, and nothing
// reaps it. Counting that idle shell as busy postponed every self-update for
// the rest of the agent's life (S17, 10-02: 5090 stuck on v0.17.0 since 09-30).
// After the command finishes, the update gate must be open again.
func TestSelfUpdateIsNotBlockedByAnIdleShellAfterExec(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	a := &Agent{sessions: map[string]*server.ShellSession{}, jobs: newJobStore()}
	sess, err := server.NewShellSession("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sess.Close)
	a.sessions["SHA256:controller"] = sess
	if code, err := sess.ExecInDir("echo hi", "", io.Discard); err != nil || code != 0 {
		t.Fatalf("exec = %d, %v", code, err)
	}
	if a.Busy() {
		t.Fatal("the agent stays busy after its one exec finished, so self-update is postponed forever")
	}
}
