package server

import (
	"os/exec"
	"syscall"
)

// createNoWindow is CREATE_NO_WINDOW. The child still gets a console — console
// APIs and taskkill's /T process-tree walk keep working — but that console
// never gets a window.
const createNoWindow = 0x08000000

// hideConsole keeps a child from flashing a console window on the device's
// desktop. The agent is started detached (DETACHED_PROCESS, see
// detachSysProcAttr), so it owns no console; a console application spawned by a
// console-less parent allocates a fresh console of its own, and in an
// interactive session that console comes with a visible window. Every child the
// agent starts on a controller's behalf must therefore ask for a console
// without a window. All stdio is piped, so nothing is lost.
//
// Existing SysProcAttr fields are preserved: the flag is OR'd in.
func hideConsole(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
