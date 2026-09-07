package server

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// A one-shot command must be cancellable: without an armed Cancel hook, a
// context that fires only ends the wait, and the shell (with the command under
// it) keeps running on the device after the controller has gone (#37).
func TestConfigureCommandCancellationArmsCancelHook(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "exit")
	configureCommandCancellation(cmd)
	if cmd.Cancel == nil {
		t.Fatal("Cancel hook not set")
	}
	if cmd.WaitDelay <= 0 {
		t.Errorf("WaitDelay = %v, want a bounded wait after the kill", cmd.WaitDelay)
	}
}

// Cancelling a command that never started is not a failure: os/exec reads
// ErrProcessDone as "nothing left to kill" and reports the command's own
// outcome instead of this.
func TestCancelBeforeStartReportsProcessDone(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "exit")
	configureCommandCancellation(cmd)
	if err := cmd.Cancel(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Cancel before Start = %v, want os.ErrProcessDone", err)
	}
}

// The kill has to take the whole tree with it. The agent starts powershell,
// which starts the user's command, which Windows may give its own console host;
// killing the shell alone leaves both of those running.
func TestTaskkillArgsKillTheWholeTree(t *testing.T) {
	if got := strings.Join(taskkillArgs(4321), " "); got != "/PID 4321 /T /F" {
		t.Fatalf("taskkillArgs = %q, want the pid with /T /F", got)
	}
}
