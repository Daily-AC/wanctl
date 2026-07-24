//go:build windows

package main

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// processAlive checks the process handle instead of relying on os.FindProcess,
// which succeeds for stale positive pids on Windows.
func processAlive(pid int) bool {
	const stillActive = 259
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var exitCode uint32
	if err := windows.GetExitCodeProcess(h, &exitCode); err != nil {
		return false
	}
	return exitCode == stillActive
}

// detachSysProcAttr truly detaches the child on Windows: DETACHED_PROCESS drops
// the parent console (so closing the launching terminal/SSH session doesn't take
// the agent down) and CREATE_NEW_PROCESS_GROUP isolates it from Ctrl-C. Without
// these the child shares the console and dies with it. For survival across
// logout/reboot use `wanctl service install` (a Scheduled Task).
func detachSysProcAttr() *syscall.SysProcAttr {
	const (
		detachedProcess       = 0x00000008
		createNewProcessGroup = 0x00000200
	)
	return &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: detachedProcess | createNewProcessGroup,
	}
}

// terminatePID kills the background agent.
func terminatePID(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
