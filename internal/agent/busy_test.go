package agent

import (
	"io"
	"runtime"
	"testing"
	"time"

	"wanctl/internal/console"
	"wanctl/internal/policy"
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

// The portal's approval watch keeps a console open on every device it watches,
// around the clock (ADR 0015). Counting the connection as work kept each of
// those devices busy, and so off every release, for as long as the watch ran
// (S19, 10-02). What a console is for is an owner answering a request, and only
// that wait is busy: once it is approved, denied or expired, the gate opens.
// main.go hands exactly this method to the auto-updater, and
// TestAutoUpdaterWaitsForIdle shows its next check installs once the gate is
// open.
func TestBusyFollowsWhatWaitsOnTheOwnerNotTheConsole(t *testing.T) {
	a := newOptsAgent(t, Options{})
	portal, msgs := openConsole(t, a)
	nextIsState(t, portal, msgs) // the console is being served
	if a.Busy() {
		t.Fatal("an open console with nothing waiting on the owner counts as busy")
	}

	ask := func() (id string, answered <-chan policy.Decision) {
		t.Helper()
		ch := make(chan policy.Decision, 1)
		go func() {
			ch <- a.console.Ask(policy.Request{Kind: policy.KindExec, Cmd: "make deploy", Peer: "SHA256:ctl"})
		}()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if p := a.console.State().Pending; len(p) == 1 {
				return p[0].ID, ch
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("the approval request never became pending")
		return "", nil
	}
	settled := func(answered <-chan policy.Decision, how string) {
		t.Helper()
		select {
		case <-answered:
		case <-time.After(15 * time.Second):
			t.Fatalf("the request was never %s", how)
		}
		if a.Busy() {
			t.Fatalf("still busy after the request was %s", how)
		}
	}

	for _, verdict := range []string{"y", "n"} {
		id, answered := ask()
		if !a.Busy() {
			t.Fatalf("a request waiting for approval (to be answered %q) does not count as busy", verdict)
		}
		a.console.Decide(id, verdict)
		settled(answered, "answered "+verdict)
	}

	a.console.SetTimeout(console.MinTimeout)
	_, answered := ask()
	if !a.Busy() {
		t.Fatal("a request waiting for approval does not count as busy")
	}
	settled(answered, "left to expire")

	if _, err := a.console.AskPairNonBlocking("SHA256:newcontroller", "laptop", ""); err != nil {
		t.Fatal(err)
	}
	if !a.Busy() {
		t.Fatal("a controller waiting to be paired does not count as busy")
	}
	a.console.DecidePair("SHA256:newcontroller", false)
	if a.Busy() {
		t.Fatal("still busy after the pairing was decided")
	}
}
