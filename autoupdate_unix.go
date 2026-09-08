//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"

	"wanctl/internal/config"
)

// restartAgentForUpdate replaces this process image with the binary that was
// just installed, keeping the pid.
//
// Keeping the pid is the whole point. Every supervisor this agent may be
// running under tracks it by pid: systemd's MAINPID, launchd's child, the
// `__supervise` parent, and wanctl's own pid file that `wanctl status` and
// `wanctl stop` read. Exiting and letting something restart us would work under
// two of those and orphan the device under the others, so the agent re-execs
// itself instead and every one of those references stays true.
//
// The agent lock is released first. It would be released anyway — Go opens
// files with O_CLOEXEC, so the descriptor does not survive the exec — but doing
// it here is what makes the new image's own AcquireAgentLock provably able to
// succeed rather than dependent on that detail.
//
// os.Args is passed through unchanged so the successor runs with the flags this
// agent was given, and fds 1 and 2 are inherited, so the log file keeps
// receiving output across the swap.
func restartAgentForUpdate(self string, args []string, lock *config.AgentLock) (bool, error) {
	_ = lock.Close()
	err := syscall.Exec(self, args, os.Environ())
	// Exec does not return on success.
	return false, fmt.Errorf("exec %s: %w", self, err)
}
