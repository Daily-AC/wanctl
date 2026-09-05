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
		kill := exec.Command("taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F")
		hideConsole(kill) // taskkill is a console app too, and would flash its own window
		if err := kill.Run(); err != nil {
			_ = cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
}
