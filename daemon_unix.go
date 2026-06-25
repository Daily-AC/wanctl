//go:build !windows

package main

import "syscall"

// processAlive reports whether a process with the given pid is running. Signal 0
// probes existence without affecting the process.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// detachSysProcAttr starts the child in a new session so it survives the parent
// (the launching `wanctl`) exiting and detaches from the controlling terminal.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// terminatePID asks the background agent to shut down (it handles SIGTERM).
func terminatePID(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}
