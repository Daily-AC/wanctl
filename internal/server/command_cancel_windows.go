package server

import (
	"os"
	"os/exec"
	"strconv"
	"time"
)

func configureCommandCancellation(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		kill := exec.Command("taskkill", taskkillArgs(cmd.Process.Pid)...)
		hideConsole(kill) // taskkill is a console app too, and would flash its own window
		if err := kill.Run(); err != nil {
			_ = cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
}

// taskkillArgs ends the whole tree rooted at pid: /T so the shell's children
// (the command the user actually ran, and the console host Windows attaches to
// it) go with the shell, and /F because a non-interactive shell has no message
// loop to answer a polite close request.
func taskkillArgs(pid int) []string {
	return []string{"/PID", strconv.Itoa(pid), "/T", "/F"}
}
