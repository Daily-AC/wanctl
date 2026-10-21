//go:build !windows

package main

// Unix re-execs in place, including foreground, launchd, systemd and __supervise.
func portalRestartSupported() error { return nil }
