//go:build windows

package main

import (
	"fmt"
	"os"

	"wanctl/internal/config"
)

// restartAgentForUpdate hands this agent's job to the binary that was just
// installed. Windows has no exec that replaces a process image, so which of the
// two shapes below applies depends on who started this agent.
//
// It reports true when a successor process already owns the pid files, so the
// caller must not clean them up on the way out.
func restartAgentForUpdate(self string, osArgs []string, lock *config.AgentLock) (bool, error) {
	// Under a supervisor — the Scheduled Task's `__supervise` loop — exiting
	// cleanly is the restart: the parent re-runs the stable binary path three
	// seconds later and gets the new file. Spawning our own successor here
	// would leave the supervisor to start a second one.
	if config.ManagedPID() == os.Getpid() {
		return false, nil
	}

	// Detached (`wanctl` / `wanctl start`): nothing will restart us, so start
	// the replacement the same way cmdStart does — same detach flags, same log
	// file, and the flags this agent was given — and let it register itself.
	// replaceBinary has already renamed the running .exe to .old, so this
	// process is unaffected and self now names the new file.
	logPath, err := config.LogPath()
	if err != nil {
		return false, err
	}
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return false, fmt.Errorf("open log: %w", err)
	}
	defer logf.Close()

	// The lock and the pid file are released before the successor starts, or it
	// would find the config dir still occupied and stand down.
	_ = lock.Close()
	_ = config.RemovePID()

	cmd := selfCommand(self, append([]string{"agent"}, successorArgs(osArgs)...)...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = detachSysProcAttr()
	if err := cmd.Start(); err != nil {
		return false, fmt.Errorf("start updated agent: %w", err)
	}
	pid := cmd.Process.Pid // captured before Release() zeroes it
	_ = config.WritePID(pid)
	_ = cmd.Process.Release()
	return true, nil
}
