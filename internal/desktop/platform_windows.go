//go:build windows

package desktop

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"syscall"

	"golang.org/x/sys/windows"
	"wanctl/internal/protocol"
)

const Supported = true

func helperProcessAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}

func executeJob(ctx context.Context, job Job) (res protocol.DesktopResult, data []byte) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	res.Status = "rejected"
	res.FailedIndex = -1
	// Keep the helper alive long enough to report a failure after releasing any
	// input through Engine's defers. Never include a panic value (possibly text).
	defer func() {
		if recover() != nil {
			res = protocol.DesktopResult{Status: "unknown", Error: "state unknown: desktop helper crashed; do not replay", FailedIndex: -1}
			data = nil
		}
	}()
	if err := setDPI(); err != nil {
		res.Error = err.Error()
		return
	}
	return executeJobWithBackend(ctx, job, &nativeBackend{}, func() error {
		_, err := desktopSession()
		return err
	})
}

func setDPI() error {
	p := user32.NewProc("SetProcessDpiAwarenessContext")
	if err := p.Find(); err != nil {
		return errors.New("desktop requires Windows 10 with Per-Monitor-V2 DPI support")
	}
	ok, _, _ := p.Call(^uintptr(3)) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 (-4)
	if ok == 0 {
		return errors.New("cannot enable Per-Monitor-V2 DPI awareness")
	}
	// WDA_EXCLUDEFROMCAPTURE only works since Windows 10 2004. Older releases
	// silently interpret it as WDA_MONITOR and would return a black rectangle.
	version := windows.RtlGetVersion()
	if version.MajorVersion < 10 || version.BuildNumber < 19041 {
		return errors.New("desktop requires Windows 10 version 2004 or newer")
	}
	return nil
}
