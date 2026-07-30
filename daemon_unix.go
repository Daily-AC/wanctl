//go:build !windows

package main

import (
	"errors"
	"syscall"
)

// processAlive reports whether a process with the given pid is running. Signal 0
// probes existence without affecting the process.
//
// EPERM counts as alive: the process exists, we simply may not signal it. An
// agent started by a supervisor running as another account (systemd as root, a
// Scheduled Task as SYSTEM) is very much running, and treating it as dead made
// `wanctl status` report "not running" and `wanctl update` skip the restart it
// owed while claiming success.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// canTerminatePID reports whether this process may signal pid — false for a live
// process owned by another account, which is exactly when an update must stop
// and tell the user to restart the supervisor instead of pretending it did.
func canTerminatePID(pid int) bool {
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
