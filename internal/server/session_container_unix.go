//go:build !windows

package server

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

// sessionContainer is the shell's process group. Setpgid with Pgid 0 makes the
// shell the leader of a new group whose id is its own pid, so everything it
// forks joins that group and a single signal to -pgid reaches all of it.
type sessionContainer struct{ pgid int }

// prepareSessionContainer runs before cmd.Start. It adds to SysProcAttr rather
// than replacing it, because hideConsole may already have set fields there.
func prepareSessionContainer(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pgid = 0
}

// captureSessionContainer runs after cmd.Start. With Setpgid/Pgid 0 the kernel
// guarantees the new group id is the child's pid, so there is nothing to look
// up and no window in which the answer could be wrong.
func captureSessionContainer(cmd *exec.Cmd) (*sessionContainer, error) {
	if cmd.Process == nil {
		return nil, errors.New("session shell has not started")
	}
	return &sessionContainer{pgid: cmd.Process.Pid}, nil
}

// Kill ends the shell and every process still in its group.
func (c *sessionContainer) Kill() error {
	if c == nil || c.pgid <= 0 {
		return nil
	}
	if err := syscall.Kill(-c.pgid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil // the group is already empty, which is the goal
		}
		return fmt.Errorf("kill session process group %d: %w", c.pgid, err)
	}
	return nil
}

// Close has nothing to release: a process group is not a handle.
func (c *sessionContainer) Close() error { return nil }
