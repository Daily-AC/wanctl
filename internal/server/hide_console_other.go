//go:build !windows

package server

import "os/exec"

// hideConsole is a no-op everywhere but Windows, the only platform where a
// child process can put a console window on someone's desktop.
func hideConsole(*exec.Cmd) {}
