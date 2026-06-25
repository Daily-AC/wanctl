//go:build windows

package main

import (
	"os"
	"syscall"
)

// processAlive is best-effort on Windows: signal-0 liveness probing isn't
// available without extra syscalls, so a recorded pid is assumed alive. The
// team runs devices on macOS/Linux; this keeps the Windows binary building and
// usable for the common case.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.FindProcess(pid)
	return err == nil
}

// detachSysProcAttr: the default attributes are sufficient to launch a detached
// child on Windows for our purposes.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

// terminatePID kills the background agent.
func terminatePID(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
