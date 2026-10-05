//go:build windows

package desktop

import (
	"context"
	"errors"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	wmInput              = 0x00ff
	ridHeader            = 0x10000005
	ridevInputSink       = 0x100
	ridevRemove          = 1
	inputMonitorInterval = 5 * time.Millisecond
)

type rawInputDevice struct {
	UsagePage, Usage uint16
	Flags            uint32
	Target           uintptr
}
type inputMessageSource struct{ Device, Origin uint32 }

type rawInputHeader struct {
	Type, Size     uint32
	Device, WParam uintptr
}

// WatchInput runs a separate message-only Raw Input sink for this batch. The
// existing banner and its hooks stay unchanged. Hook callbacks can be silently
// removed by Windows on timeout while that banner's heartbeat still advances;
// WM_INPUT comes from the device path instead of depending on that hook chain.
// https://learn.microsoft.com/windows/win32/winmsg/lowlevelmouseproc
func (b *nativeBackend) WatchInput(sig *Signal) (func(), error) {
	b.inputSignal = sig
	stop, done := make(chan struct{}), make(chan struct{})
	ready := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(done)
		orderly := false
		defer func() {
			if !orderly {
				sig.failMonitor()
			}
		}()
		if _, err := desktopSession(); err != nil {
			ready <- err
			return
		}
		// Resolve APIs before accepting input; the event callback does only a
		// bounded header read, one boolean store and the required OS cleanup.
		getRaw := user32.NewProc("GetRawInputData")
		defWindow := user32.NewProc("DefWindowProcW")
		getSource := user32.NewProc("GetCurrentInputMessageSource")
		getExtra := user32.NewProc("GetMessageExtraInfo")
		peek := user32.NewProc("PeekMessageW")
		dispatch := user32.NewProc("DispatchMessageW")
		for _, proc := range []*windows.LazyProc{getRaw, defWindow, getSource, getExtra, peek, dispatch} {
			if err := proc.Find(); err != nil {
				ready <- errors.New("Raw Input API unavailable")
				return
			}
		}
		proc := windows.NewCallback(func(hwnd uintptr, message uint32, wparam, lparam uintptr) uintptr {
			if message == wmInput {
				var source inputMessageSource
				sourceOK, _, _ := getSource.Call(uintptr(unsafe.Pointer(&source)))
				extra, _, _ := getExtra.Call()
				var header rawInputHeader
				size := uint32(unsafe.Sizeof(header))
				copied, _, _ := getRaw.Call(lparam, ridHeader, uintptr(unsafe.Pointer(&header)), uintptr(unsafe.Pointer(&size)), unsafe.Sizeof(header))
				if copied != unsafe.Sizeof(header) || sourceOK == 0 {
					sig.failMonitor()
				} else {
					// RID_HEADER does NOT fetch RAWKEYBOARD/RAWMOUSE: no key code, typed
					// content, button or pointer coordinates are read or retained. Do not
					// reject a zero hDevice: precision touchpads legitimately use it.
					sig.RawInputEvent(header.Type, source.Origin, extra)
				}
				defWindow.Call(hwnd, uintptr(message), wparam, lparam)
				return 0
			}
			ret, _, _ := defWindow.Call(hwnd, uintptr(message), wparam, lparam)
			return ret
		})
		var module windows.Handle
		if windows.GetModuleHandleEx(0, nil, &module) != nil {
			ready <- errors.New("cannot read helper module")
			return
		}
		name := utf16ptr("WanctlPhysicalInputMonitor")
		class := windowClass{Size: uint32(unsafe.Sizeof(windowClass{})), WndProc: proc, Instance: uintptr(module), ClassName: name}
		atom, _, _ := user32.NewProc("RegisterClassExW").Call(uintptr(unsafe.Pointer(&class)))
		if atom == 0 {
			ready <- errors.New("cannot register physical input monitor")
			return
		}
		defer user32.NewProc("UnregisterClassW").Call(uintptr(unsafe.Pointer(name)), uintptr(module))
		// HWND_MESSAGE (-3): invisible, never activates, no banner/UI alteration.
		hwnd, _, _ := user32.NewProc("CreateWindowExW").Call(0, uintptr(unsafe.Pointer(name)), 0, 0, 0, 0, 0, 0, ^uintptr(2), 0, uintptr(module), 0)
		if hwnd == 0 {
			ready <- errors.New("cannot create physical input monitor")
			return
		}
		defer user32.NewProc("DestroyWindow").Call(hwnd)
		devices := [2]rawInputDevice{{UsagePage: 1, Usage: 2, Flags: ridevInputSink, Target: hwnd}, {UsagePage: 1, Usage: 6, Flags: ridevInputSink, Target: hwnd}}
		register := user32.NewProc("RegisterRawInputDevices")
		if callOK(register, uintptr(unsafe.Pointer(&devices[0])), uintptr(len(devices)), unsafe.Sizeof(devices[0])) != nil {
			ready <- errors.New("cannot monitor physical keyboard and mouse input")
			return
		}
		defer func() {
			for i := range devices {
				devices[i].Flags = ridevRemove
				devices[i].Target = 0
			}
			register.Call(uintptr(unsafe.Pointer(&devices[0])), uintptr(len(devices)), unsafe.Sizeof(devices[0]))
		}()
		pump := func() bool {
			// Bounded draining also lets shutdown and health checks run under a burst.
			for i := 0; i < 256; i++ {
				var msg winMessage
				available, _, _ := peek.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0, 1)
				if available == 0 {
					return true
				}
				if msg.Message == 0x0012 {
					return false
				}
				dispatch.Call(uintptr(unsafe.Pointer(&msg)))
			}
			return true
		}
		b.inputAlive.Store(time.Now().UnixMilli())
		ready <- nil
		ticker := time.NewTicker(inputMonitorInterval)
		defer ticker.Stop()
		for {
			if !pump() {
				return
			}
			b.inputAlive.Store(time.Now().UnixMilli())
			select {
			case <-stop:
				// Observe already-queued physical input before Engine finalizes an error.
				orderly = pump()
				return
			case <-ticker.C:
			}
		}
	}()
	if err := <-ready; err != nil {
		<-done
		return nil, err
	}
	return func() { close(stop); <-done }, nil
}

func (b *nativeBackend) physicalInputError() error {
	if b.inputSignal == nil {
		return nil
	} // screenshot-only helper
	if time.Since(time.UnixMilli(b.inputAlive.Load())) > 100*time.Millisecond {
		b.inputSignal.failMonitor()
	}
	return b.inputSignal.Check(context.Background())
}
