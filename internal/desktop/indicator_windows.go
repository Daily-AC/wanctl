//go:build windows

package desktop

import (
	"errors"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowClass struct {
	Size, Style                        uint32
	WndProc                            uintptr
	ClsExtra, WndExtra                 int32
	Instance, Icon, Cursor, Background uintptr
	MenuName, ClassName                *uint16
	IconSmall                          uintptr
}
type winMessage struct {
	Hwnd           uintptr
	Message        uint32
	WParam, LParam uintptr
	Time           uint32
	Point          winPoint
	Private        uint32
}
type mouseHook struct {
	Point             winPoint
	Data, Flags, Time uint32
	Extra             uintptr
}
type keyboardHook struct {
	VK, Scan, Flags, Time uint32
	Extra                 uintptr
}

func (b *nativeBackend) Begin(controller string, sig *Signal) (func(), error) {
	stopped := make(chan struct{})
	done := make(chan struct{})
	ready := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(done)
		if _, err := desktopSession(); err != nil {
			ready <- err
			return
		}
		// Only flags are inspected. No key codes, scan codes, pointer coordinates,
		// or input content are copied into application state, logs or IPC.
		keyboardCB := windows.NewCallback(func(code int32, wparam uintptr, event *keyboardHook) uintptr {
			if code >= 0 {
				sig.HumanInput(event.Flags&0x10 != 0)
			} // LLKHF_INJECTED
			r, _, _ := user32.NewProc("CallNextHookEx").Call(0, uintptr(code), wparam, uintptr(unsafe.Pointer(event)))
			return r
		})
		mouseCB := windows.NewCallback(func(code int32, wparam uintptr, event *mouseHook) uintptr {
			if code >= 0 {
				sig.HumanInput(event.Flags&1 != 0)
			} // LLMHF_INJECTED
			r, _, _ := user32.NewProc("CallNextHookEx").Call(0, uintptr(code), wparam, uintptr(unsafe.Pointer(event)))
			return r
		})
		var module windows.Handle
		if windows.GetModuleHandleEx(0, nil, &module) != nil {
			ready <- errors.New("cannot read helper module")
			return
		}
		keyboardHandle, _, _ := user32.NewProc("SetWindowsHookExW").Call(13, keyboardCB, uintptr(module), 0)
		if keyboardHandle == 0 {
			ready <- errors.New("cannot monitor real keyboard input")
			return
		}
		defer user32.NewProc("UnhookWindowsHookEx").Call(keyboardHandle)
		mouseHandle, _, _ := user32.NewProc("SetWindowsHookExW").Call(14, mouseCB, uintptr(module), 0)
		if mouseHandle == 0 {
			ready <- errors.New("cannot monitor real mouse input")
			return
		}
		defer user32.NewProc("UnhookWindowsHookEx").Call(mouseHandle)
		// A key held before hook installation must also refuse the batch. Retain
		// only a boolean; never release a person's already-held keys.
		for key := 1; key < 256; key++ {
			state, _, _ := user32.NewProc("GetAsyncKeyState").Call(uintptr(key))
			if state&0x8000 != 0 {
				sig.HumanInput(false)
				break
			}
		}
		wndproc := windows.NewCallback(func(hwnd uintptr, msg uint32, wparam, lparam uintptr) uintptr {
			switch msg {
			case 0x0021:
				return 3 // WM_MOUSEACTIVATE -> MA_NOACTIVATE
			case 0x0010:
				sig.HumanInput(false)
				return 0 // WM_CLOSE
			case 0x0111:
				if wparam&0xffff == 1 {
					sig.HumanInput(false)
					return 0
				} // stop button
			}
			r, _, _ := user32.NewProc("DefWindowProcW").Call(hwnd, uintptr(msg), wparam, lparam)
			return r
		})
		className := utf16ptr("WanctlDesktopActIndicator")
		class := windowClass{Size: uint32(unsafe.Sizeof(windowClass{})), WndProc: wndproc, Instance: uintptr(module), Background: 6, ClassName: className}
		cursor, _, _ := user32.NewProc("LoadCursorW").Call(0, 32512)
		class.Cursor = cursor
		atom, _, _ := user32.NewProc("RegisterClassExW").Call(uintptr(unsafe.Pointer(&class)))
		if atom == 0 {
			ready <- errors.New("cannot register visible indicator")
			return
		}
		defer user32.NewProc("UnregisterClassW").Call(uintptr(unsafe.Pointer(className)), uintptr(module))
		snap, err := displayState()
		if err != nil {
			ready <- err
			return
		}
		var banners, fonts []uintptr
		defer func() {
			b.banner.Store(0)
			for _, h := range banners {
				user32.NewProc("DestroyWindow").Call(h)
			}
			for _, font := range fonts {
				gdi32.NewProc("DeleteObject").Call(font)
			}
		}()
		controller = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, controller)
		if r := []rune(controller); len(r) > 80 {
			controller = string(r[:80]) + "…"
		}
		if controller == "" {
			controller = "wanctl controller"
		}
		for _, m := range snap.Monitors {
			scale := func(n int) int { return n * m.DPI / 96 }
			width := min(scale(720), m.Rect.Width-scale(24))
			height := scale(100)
			x, y := m.Rect.X+(m.Rect.Width-width)/2, m.Rect.Y+scale(12)
			hwnd, _, _ := user32.NewProc("CreateWindowExW").Call(0x08000088, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(utf16ptr("wanctl desktop control"))), 0x80800000, uintptr(x), uintptr(y), uintptr(width), uintptr(height), 0, 0, uintptr(module), 0)
			if hwnd == 0 {
				ready <- errors.New("cannot create visible indicator")
				return
			}
			banners = append(banners, hwnd)
			// Both setting and reading back the affinity must succeed before input.
			if callOK(user32.NewProc("SetWindowDisplayAffinity"), hwnd, 0x11) != nil {
				ready <- errors.New("cannot exclude indicator from screenshots")
				return
			}
			var affinity uint32
			if callOK(user32.NewProc("GetWindowDisplayAffinity"), hwnd, uintptr(unsafe.Pointer(&affinity))) != nil || affinity != 0x11 {
				ready <- errors.New("indicator screenshot exclusion is unavailable")
				return
			}
			label := controller + " 正在操作这台电脑\n移动鼠标或按键立即停止 · Moving the mouse stops control"
			text, _, _ := user32.NewProc("CreateWindowExW").Call(0, uintptr(unsafe.Pointer(utf16ptr("STATIC"))), uintptr(unsafe.Pointer(utf16ptr(label))), 0x50000001, uintptr(scale(12)), uintptr(scale(12)), uintptr(width-scale(24)), uintptr(scale(44)), hwnd, 0, uintptr(module), 0)
			button, _, _ := user32.NewProc("CreateWindowExW").Call(0, uintptr(unsafe.Pointer(utf16ptr("BUTTON"))), uintptr(unsafe.Pointer(utf16ptr("停止 / Stop"))), 0x50000000, uintptr((width-scale(140))/2), uintptr(scale(62)), uintptr(scale(140)), uintptr(scale(28)), hwnd, 1, uintptr(module), 0)
			if text == 0 || button == 0 {
				ready <- errors.New("cannot create indicator text or stop button")
				return
			}
			font, _, _ := gdi32.NewProc("CreateFontW").Call(uintptr(-scale(16)), 0, 0, 0, 400, 0, 0, 0, 1, 0, 0, 5, 0, uintptr(unsafe.Pointer(utf16ptr("Segoe UI"))))
			if font != 0 {
				fonts = append(fonts, font)
				user32.NewProc("SendMessageW").Call(text, 0x0030, font, 1)
				user32.NewProc("SendMessageW").Call(button, 0x0030, font, 1)
			}
			user32.NewProc("ShowWindow").Call(hwnd, 4) // SW_SHOWNOACTIVATE
			if callOK(user32.NewProc("SetWindowPos"), hwnd, ^uintptr(0), 0, 0, 0, 0, 0x0013) != nil {
				ready <- errors.New("cannot show topmost indicator")
				return
			} // TOPMOST, NOMOVE|NOSIZE|NOACTIVATE
			visible, _, _ := user32.NewProc("IsWindowVisible").Call(hwnd)
			if visible == 0 {
				ready <- errors.New("indicator is not visible")
				return
			}
		}
		b.monitorAlive.Store(time.Now().UnixMilli())
		b.banner.Store(banners[0])
		b.monitorRequired.Store(true)
		ready <- nil
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			var msg winMessage
			for {
				available, _, _ := user32.NewProc("PeekMessageW").Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0, 1)
				if available == 0 {
					break
				}
				if msg.Message == 0x0012 {
					sig.HumanInput(false)
					return
				} // WM_QUIT must fail closed.
				user32.NewProc("TranslateMessage").Call(uintptr(unsafe.Pointer(&msg)))
				user32.NewProc("DispatchMessageW").Call(uintptr(unsafe.Pointer(&msg)))
			}
			b.monitorAlive.Store(time.Now().UnixMilli())
			select {
			case <-stopped:
				return
			case <-ticker.C:
			}
		}
	}()
	err := <-ready
	if err != nil {
		<-done
		return nil, err
	}
	return func() { close(stopped); <-done }, nil
}
