//go:build windows

package main

import (
	"context"
	"fmt"
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

// canTerminatePID reports whether this process may terminate pid. A Scheduled
// Task running the agent as SYSTEM is queryable by any user but killable only
// with elevation, which is exactly when an update must stop and say so instead
// of pretending it restarted the agent.
func canTerminatePID(pid int) bool {
	if pid <= 0 {
		return false
	}
	enableDebugPrivilege()
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return false
	}
	windows.CloseHandle(h)
	return true
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
//
// It opens the process for PROCESS_TERMINATE itself, the same open
// canTerminatePID tries, instead of using os.FindProcess and Kill. Those open
// the process without terminate access and then ask for it by duplicating the
// handle, and that second access check ignores SeDebugPrivilege. A SYSTEM
// agent's process grants administrators no terminate access, only the
// privilege lets them in, so an elevated `wanctl update` passed the check and
// its restart helper then failed with "DuplicateHandle: Access is denied",
// leaving the old build serving (S17, S18; diagnosed on zyl 2026-10-02).
func terminatePID(pid int) error {
	enableDebugPrivilege()
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("open process %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, 1)
}

// enableDebugPrivilege switches on SeDebugPrivilege when this process's token
// holds it. An elevated administrator's token does, but disabled, except in an
// OpenSSH session; it is what opens a SYSTEM agent for termination. A token
// without it is left as it is, and the open that follows fails.
func enableDebugPrivilege() {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &token); err != nil {
		return
	}
	defer token.Close()
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, windows.StringToUTF16Ptr("SeDebugPrivilege"), &luid); err != nil {
		return
	}
	privs := windows.Tokenprivileges{PrivilegeCount: 1}
	privs.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
	_ = windows.AdjustTokenPrivileges(token, false, &privs, 0, nil, nil)
}

// exitWithParent cancels the supervisor when the process that started it exits.
// Under the logon task that parent is the headless conhost, and ending the task
// (`schtasks /end`, part of `service uninstall`) terminates only that conhost:
// without this the supervisor outlives it and keeps restarting the agent.
func exitWithParent(cancel context.CancelFunc) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(os.Getppid()))
	if err != nil {
		return
	}
	go func() {
		defer windows.CloseHandle(h)
		_, _ = windows.WaitForSingleObject(h, windows.INFINITE)
		cancel()
	}()
}
