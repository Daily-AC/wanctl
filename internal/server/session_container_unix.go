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
//   - No signal is sent after the shell has been reaped. Until then the shell
//     is at worst a zombie, and the kernel does not reuse a group leader's pid
//     while its zombie exists, so the number still names this group and nothing
//     else. reap is called the moment the kernel has waited the shell — not
//     when cmd.Wait later returns from copying output — under the same lock
//     the kill takes, so the two cannot interleave.
type sessionContainer struct {
	mu     sync.Mutex
	pgid   int
	killed bool
	reaped bool
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
// the first call and after the shell has been reaped; see the type comment for
// why both are required rather than merely tidy.
func (c *sessionContainer) Kill() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pgid <= 0 || c.killed || c.reaped {
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

// reap records that the shell has been waited on, so its pid — and the group id
// that is the same number — may now belong to something else.
func (c *sessionContainer) reap() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.reaped = true
	c.pgid = 0
	c.mu.Unlock()
}

// Close has nothing to release: a process group is not a handle.
func (c *sessionContainer) Close() error { return nil }
