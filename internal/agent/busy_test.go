package agent

import (
	"runtime"
	"testing"

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

	t.Run("an open shell session", func(t *testing.T) {
		sess, err := server.NewShellSession("/bin/sh")
		if err != nil {
			t.Fatal(err)
		}
		a.sessions["SHA256:controller"] = sess
		if !a.Busy() {
			t.Fatal("an open shell session does not count as busy")
		}
		sess.Close()
		// A closed session is history: its cwd and environment are already
		// gone, so nothing is lost by restarting around it.
		if a.Busy() {
			t.Fatal("a closed session still counts as busy")
		}
	})

	t.Run("a running background job", func(t *testing.T) {
		if _, err := a.jobs.start("/bin/sh", "sleep 5", ""); err != nil {
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
