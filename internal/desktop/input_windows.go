//go:build windows

package desktop

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"wanctl/internal/protocol"
)

type mouseInput struct {
	DX, DY            int32
	Data, Flags, Time uint32
	Extra             uintptr
}
type keyboardInput struct {
	VK, Scan    uint16
	Flags, Time uint32
	Extra       uintptr
}

// The MOUSEINPUT member supplies INPUT's union size and pointer alignment on
// both 32- and 64-bit Windows. Keyboard input occupies its first bytes.
type winInput struct {
	Type  uint32
	Mouse mouseInput
}

func sendNative(in *winInput) error {
	n, _, _ := user32.NewProc("SendInput").Call(1, uintptr(unsafe.Pointer(in)), unsafe.Sizeof(*in))
	if n != 1 {
		return errors.New("input was not delivered; target may be elevated or desktop unavailable")
	}
	return nil
}
func (b *nativeBackend) Move(p protocol.Point) error {
	if _, err := b.uncoveredRoot(p); err != nil {
		return err
	}
	r := b.bounds
	if r.Width <= 0 || r.Height <= 0 {
		return errors.New("invalid virtual desktop geometry")
	}
	// Pixel centers, normalized over the entire virtual desktop, including
	// negative origins. MOUSEEVENTF_VIRTUALDESK is essential on secondary screens.
	dx := ((int64(p.X-r.X)*2 + 1) * 65536) / (int64(r.Width) * 2)
	dy := ((int64(p.Y-r.Y)*2 + 1) * 65536) / (int64(r.Height) * 2)
	return sendNative(&winInput{Mouse: mouseInput{DX: int32(dx), DY: int32(dy), Flags: 0x0001 | 0x8000 | 0x4000, Extra: ownInputMarker}})
}
func (b *nativeBackend) Button(button string, down bool) error {
	var flag uint32
	switch button {
	case "left":
		flag = 0x0002
	case "right":
		flag = 0x0008
	case "middle":
		flag = 0x0020
	default:
		return errors.New("invalid mouse button")
	}
	if !down {
		flag *= 2
	}
	return sendNative(&winInput{Mouse: mouseInput{Flags: flag, Extra: ownInputMarker}})
}
func (b *nativeBackend) Wheel(delta int) error {
	return sendNative(&winInput{Mouse: mouseInput{Data: uint32(int32(delta * 120)), Flags: 0x0800, Extra: ownInputMarker}})
}
func keyboard(scan uint16, flags uint32) error {
	in := winInput{Type: 1}
	*(*keyboardInput)(unsafe.Pointer(&in.Mouse)) = keyboardInput{Scan: scan, Flags: flags, Extra: ownInputMarker}
	return sendNative(&in)
}
func (b *nativeBackend) Unicode(unit uint16, down bool) error {
	flags := uint32(0x0004)
	if !down {
		flags |= 2
	}
	return keyboard(unit, flags)
}

// Keep Unicode SendInput, but put the final live foreground check AFTER the
// engine's monitoring, metadata, lock and held-state work and all packet/API
// preparation. This shortens the race; global input delivery is not atomic.
func (b *nativeBackend) UnicodeChecked(expected protocol.DesktopWindow, unit uint16, down bool) error {
	if !down {
		return b.Unicode(unit, false) // release even after focus changes
	}
	want, err := strconv.ParseUint(expected.ID, 16, 64)
	if err != nil || want == 0 {
		return errors.New("invalid text target window")
	}
	in := winInput{Type: 1}
	*(*keyboardInput)(unsafe.Pointer(&in.Mouse)) = keyboardInput{Scan: unit, Flags: 0x0004, Extra: ownInputMarker}
	send := user32.NewProc("SendInput")
	if err = send.Find(); err != nil {
		return err
	}
	entry := send.Addr()
	var pid uint32
	user32.NewProc("GetWindowThreadProcessId").Call(uintptr(want), uintptr(unsafe.Pointer(&pid)))
	if pid != expected.PID {
		return errors.New("foreground window changed; stopped before sending input")
	}
	current, _, _ := user32.NewProc("GetForegroundWindow").Call()
	if current != uintptr(want) {
		return errors.New("foreground window changed; stopped before sending input")
	}
	n, _, _ := syscall.SyscallN(entry, 1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
	if n != 1 {
		return errors.New("input was not delivered; target may be elevated or desktop unavailable")
	}
	return nil
}
func (b *nativeBackend) Key(vk uint16, down bool) error {
	if b.keyScans == nil {
		b.keyScans = map[uint16]uintptr{}
	}
	scan := b.keyScans[vk]
	if down {
		hwnd, _, _ := user32.NewProc("GetForegroundWindow").Call()
		thread, _, _ := user32.NewProc("GetWindowThreadProcessId").Call(hwnd, 0)
		layout, _, _ := user32.NewProc("GetKeyboardLayout").Call(thread)
		scan, _, _ = user32.NewProc("MapVirtualKeyExW").Call(uintptr(vk), 4, layout)
		if scan == 0 {
			return errors.New("key has no scan code on the foreground keyboard layout")
		}
		b.keyScans[vk] = scan
	}
	if scan == 0 {
		return nil
	}
	flags := uint32(0x0008)
	if scan&0xff00 != 0 {
		flags |= 1
	}
	if !down {
		flags |= 2
	}
	err := keyboard(uint16(scan&0xff), flags)
	if !down && err == nil {
		delete(b.keyScans, vk)
	}
	return err
}

func (b *nativeBackend) Launch(a protocol.DesktopAction) (uint32, error) {
	// Direct CreateProcess via os/exec, never a shell or the exec Job Object.
	// No elevation, task, inherited pipe, or parent-death kill: the owner's
	// program deliberately survives this short-lived helper.
	cmd := exec.Command(a.Program, a.Args...)
	cmd.Dir = a.Cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP, NoInheritHandles: true}
	if err := cmd.Start(); err != nil {
		return 0, errors.New("program could not be started")
	}
	pid := uint32(cmd.Process.Pid)
	_ = cmd.Process.Release()
	return pid, nil
}

type shellExecuteInfo struct {
	Size, Mask                        uint32
	Window                            uintptr
	Verb, File, Parameters, Directory *uint16
	Show                              int32
	Instance, IDList                  uintptr
	Class                             *uint16
	ClassKey                          uintptr
	HotKey                            uint32
	Icon                              uintptr
	Process                           windows.Handle
}

func (b *nativeBackend) LaunchShortcut(a protocol.DesktopAction) (uint32, error) {
	// The helper runs on a locked OS thread. Let the Shell resolve the link's
	// target, arguments and working directory; do not parse or rewrite it.
	if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED|windows.COINIT_DISABLE_OLE1DDE); err != nil && err != syscall.Errno(1) {
		return 0, errors.New("cannot initialize shortcut launch")
	}
	defer windows.CoUninitialize()
	args := make([]string, len(a.Args))
	for i, arg := range a.Args {
		args[i] = windows.EscapeArg(arg)
	}
	parameters := strings.Join(args, " ")
	if strings.ContainsRune(a.Program+parameters+a.Cwd, 0) {
		return 0, errors.New("invalid shortcut launch argument")
	}
	info := shellExecuteInfo{Mask: 0x40 | 0x100 | 0x400, Verb: utf16ptr("open"), File: utf16ptr(a.Program), Show: 1}
	info.Size = uint32(unsafe.Sizeof(info)) // NOCLOSEPROCESS | NOASYNC | FLAG_NO_UI
	if parameters != "" {
		info.Parameters = utf16ptr(parameters)
	}
	if a.Cwd != "" {
		info.Directory = utf16ptr(a.Cwd)
	}
	ok, _, _ := windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW").Call(uintptr(unsafe.Pointer(&info)))
	if ok == 0 {
		return 0, errors.New("shortcut could not be started")
	}
	if info.Process == 0 {
		return 0, nil // an existing process may handle it; title matching still works
	}
	defer windows.CloseHandle(info.Process)
	pid, _ := windows.GetProcessId(info.Process)
	return pid, nil
}
