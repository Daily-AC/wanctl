//go:build windows

package main

import (
	"os"
	"strconv"

	"golang.org/x/sys/windows"
	"wanctl/internal/config"
)

func portalRestartSupported() error {
	var session uint32
	sessionErr := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session)
	return checkPortalRestartMode("windows", config.ManagedPID() == os.Getpid(), os.Getenv("WANCTL_SUPERVISOR_PID") == strconv.Itoa(os.Getppid()), sessionErr != nil || session == 0)
}
