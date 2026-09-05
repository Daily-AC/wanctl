package server

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestHideConsoleSetsFlags(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "exit")
	hideConsole(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr not set")
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Error("HideWindow not set")
	}
	if cmd.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Errorf("CREATE_NO_WINDOW not set, flags = %#x", cmd.SysProcAttr.CreationFlags)
	}
}

// The agent must not lose flags a caller already asked for.
func TestHideConsolePreservesExistingFlags(t *testing.T) {
	const createNewProcessGroup = 0x00000200
	cmd := exec.Command("cmd", "/c", "exit")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
	hideConsole(cmd)
	if cmd.SysProcAttr.CreationFlags&createNewProcessGroup == 0 {
		t.Error("existing creation flag was clobbered")
	}
	if cmd.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Error("CREATE_NO_WINDOW not set")
	}
}
