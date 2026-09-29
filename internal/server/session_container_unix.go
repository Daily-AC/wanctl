//go:build !windows

package server

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
)

// sessionContainer is the shell's process group. Setpgid with Pgid 0 makes the
// shell the leader of a new group whose id is its own pid, so everything it
// forks joins that group and a single signal to -pgid reaches all of it.
//
// Unlike a Windows job handle, a process-group id is only a number and carries
// no identity: once the group is gone the number can be given to another group.
// Two rules keep that from mattering, and both are enforced here rather than
// left to callers.
//
//   - The group is signalled at most once in a container's lifetime. A second
//     kill would be the one that could hit a stranger, and there is never a
//     reason for it: the first either worked or reported why not.
//   - That one signal is spent when the shell is reaped: the reaper calls Kill
//     the moment the kernel has waited the shell. Until the reap the shell is
//     at worst a zombie, and the kernel does not reuse a group leader's pid
//     while its zombie exists; after it, the number stays reserved only while
//     members are left in the group. So the reaper's kill reaches those members
//     and nothing else, except in one case: an empty group whose number the
//     kernel hands out again in the few instructions between the reap and the
//     kill. Pids are allocated cyclically, so that means wrapping the whole pid
//     space in that gap. Closing it would take waiting for the exit without
//     reaping — waitid(WNOWAIT) on Linux, a kqueue on macOS — which is not
//     worth two platform files.
type sessionContainer struct {
	mu     sync.Mutex
	pgid   int
	killed bool
}

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

// Kill ends the shell and every process still in its group. It is a no-op after
// the first call; see the type comment for why that is required rather than
// merely tidy.
func (c *sessionContainer) Kill() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pgid <= 0 || c.killed {
		return nil
	}
	c.killed = true
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
