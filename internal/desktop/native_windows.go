//go:build windows

package desktop

import (
	"encoding/binary"
	"errors"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"wanctl/internal/protocol"
)

var (
	user32 = windows.NewLazySystemDLL("user32.dll")
	gdi32  = windows.NewLazySystemDLL("gdi32.dll")
	dwmapi = windows.NewLazySystemDLL("dwmapi.dll")
)

type winRect struct{ Left, Top, Right, Bottom int32 }

func (r winRect) rect() protocol.Rect {
	return protocol.Rect{X: int(r.Left), Y: int(r.Top), Width: int(r.Right - r.Left), Height: int(r.Bottom - r.Top)}
}

type winPoint struct{ X, Y int32 }
type monitorInfo struct {
	Size          uint32
	Monitor, Work winRect
	Flags         uint32
	Device        [32]uint16
}
type nativeBackend struct {
	banner          atomic.Uintptr
	monitorAlive    atomic.Int64
	monitorRequired atomic.Bool
	lastLayoutCheck time.Time
	layout          string
	bounds          protocol.Rect
	keyScans        map[uint16]uintptr
	monitors        []protocol.DesktopMonitor
}

func utf16ptr(s string) *uint16 { p, _ := windows.UTF16PtrFromString(s); return p }
func callOK(p *windows.LazyProc, args ...uintptr) error {
	r, _, _ := p.Call(args...)
	if r == 0 {
		return errors.New(p.Name + " failed")
	}
	return nil
}

func desktopSession() (uint32, error) {
	var session uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session); err != nil {
		return 0, errors.New("cannot determine desktop session")
	}
	if session == 0 {
		return 0, errors.New("desktop unavailable: session 0 is not supported; run the agent in the logged-in user's desktop session")
	}
	if session != windows.WTSGetActiveConsoleSessionId() {
		return 0, errors.New("desktop unavailable: no active console user in this agent's session")
	}
	desk, _, _ := user32.NewProc("OpenInputDesktop").Call(0, 0, 1) // DESKTOP_READOBJECTS
	if desk == 0 {
		return 0, errors.New("desktop unavailable: screen locked or secure desktop active")
	}
	defer user32.NewProc("CloseDesktop").Call(desk)
	name := func(handle uintptr) string {
		var buf [256]uint16
		var needed uint32
		ok, _, _ := user32.NewProc("GetUserObjectInformationW").Call(handle, 2, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Sizeof(buf)), uintptr(unsafe.Pointer(&needed)))
		if ok == 0 {
			return ""
		}
		return windows.UTF16ToString(buf[:])
	}
	current, _, _ := user32.NewProc("GetThreadDesktop").Call(uintptr(windows.GetCurrentThreadId()))
	if !strings.EqualFold(name(desk), "Default") || !strings.EqualFold(name(current), "Default") {
		return 0, errors.New("desktop unavailable: screen locked or secure desktop active")
	}
	return session, nil
}

func displayState() (protocol.DesktopSnapshot, error) {
	var snap protocol.DesktopSnapshot
	session, err := desktopSession()
	if err != nil {
		return snap, err
	}
	snap.Session = session
	state := monitorEnumeration{snap: &snap, rotations: make(map[string]uint32)}

	ok, _, _ := user32.NewProc("EnumDisplayMonitors").Call(0, 0, monitorCallback, uintptr(unsafe.Pointer(&state)))
	runtime.KeepAlive(&state)
	if state.err != nil {
		return snap, state.err
	}
	if ok == 0 || len(snap.Monitors) == 0 {
		return snap, errors.New("desktop unavailable: no monitors")
	}
	sort.Slice(snap.Monitors, func(i, j int) bool { return snap.Monitors[i].ID < snap.Monitors[j].ID })
	bounds := snap.Monitors[0].Rect
	right, bottom := bounds.X+bounds.Width, bounds.Y+bounds.Height
	for _, m := range snap.Monitors {
		bounds.X = min(bounds.X, m.Rect.X)
		bounds.Y = min(bounds.Y, m.Rect.Y)
		right = max(right, m.Rect.X+m.Rect.Width)
		bottom = max(bottom, m.Rect.Y+m.Rect.Height)
	}
	bounds.Width = right - bounds.X
	bounds.Height = bottom - bounds.Y
	snap.Source = bounds
	snap.Origin = protocol.Point{X: bounds.X, Y: bounds.Y}
	snap.Layout = layoutFingerprint(session, snap.Monitors, state.rotations)
	return snap, nil
}
func (b *nativeBackend) Check(layout string) error {
	if b.monitorRequired.Load() && (b.banner.Load() == 0 || time.Since(time.UnixMilli(b.monitorAlive.Load())) > 100*time.Millisecond) {
		return errors.New("desktop safety monitor stopped responding")
	}
	if _, err := desktopSession(); err != nil {
		return err
	}
	// The input hook still runs continuously; display enumeration is bounded to
	// 50 Hz so long Unicode strings do not create thousands of DPI probes.
	if b.layout == "" || time.Since(b.lastLayoutCheck) >= 20*time.Millisecond {
		s, err := displayState()
		if err != nil {
			return err
		}
		b.layout = s.Layout
		b.bounds = s.Source
		b.monitors = s.Monitors
		b.lastLayoutCheck = time.Now()
	}
	if b.layout != layout {
		return errors.New("display configuration or session changed; take a fresh screenshot")
	}
	return nil
}
func windowInfo(hwnd uintptr) (protocol.DesktopWindow, error) {
	var w protocol.DesktopWindow
	if hwnd == 0 {
		return w, errors.New("no foreground window")
	}
	var pid uint32
	user32.NewProc("GetWindowThreadProcessId").Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 {
		return w, errors.New("window no longer exists")
	}
	w.ID = strconv.FormatUint(uint64(hwnd), 16)
	w.PID = pid
	var title [1024]uint16
	user32.NewProc("GetWindowTextW").Call(hwnd, uintptr(unsafe.Pointer(&title[0])), uintptr(len(title)))
	w.Title = windows.UTF16ToString(title[:])
	var rect winRect
	if callOK(user32.NewProc("GetWindowRect"), hwnd, uintptr(unsafe.Pointer(&rect))) != nil {
		return w, errors.New("cannot read window rectangle")
	}
	w.Rect = rect.rect()
	proc, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err == nil {
		defer windows.CloseHandle(proc)
		var path [32768]uint16
		n := uint32(len(path))
		if windows.QueryFullProcessImageName(proc, 0, &path[0], &n) == nil {
			w.Process = filepath.Base(windows.UTF16ToString(path[:n]))
		}
		var token windows.Token
		if windows.OpenProcessToken(proc, windows.TOKEN_QUERY, &token) == nil {
			var elevated, returned uint32
			if windows.GetTokenInformation(token, windows.TokenElevation, (*byte)(unsafe.Pointer(&elevated)), uint32(unsafe.Sizeof(elevated)), &returned) == nil {
				w.Elevated = elevated != 0
				w.ElevationKnown = true
			}
			token.Close()
		}
	}
	return w, nil
}
func (b *nativeBackend) Foreground() (protocol.DesktopWindow, error) {
	hwnd, _, _ := user32.NewProc("GetForegroundWindow").Call()
	return windowInfo(hwnd)
}
func (b *nativeBackend) Windows() ([]protocol.DesktopWindow, error) {
	state := windowEnumeration{backend: b}

	ok, _, _ := user32.NewProc("EnumWindows").Call(windowCallback, uintptr(unsafe.Pointer(&state)))
	runtime.KeepAlive(&state)
	if ok == 0 {
		return nil, errors.New("cannot enumerate windows")
	}
	return state.result, nil
}
func (b *nativeBackend) uncoveredRoot(p protocol.Point) (uintptr, error) {
	hwnd := windowFromPoint(int32(p.X), int32(p.Y))
	hwnd, _, _ = user32.NewProc("GetAncestor").Call(hwnd, 2) // GA_ROOT
	var pid uint32
	user32.NewProc("GetWindowThreadProcessId").Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid == windows.GetCurrentProcessId() {
		// The excluded banner must not make the screen below it permanently
		// unclickable. Move it to the opposite edge BEFORE hit testing again;
		// it stays visible, excluded from capture, and never becomes active.
		var rect winRect
		if callOK(user32.NewProc("GetWindowRect"), hwnd, uintptr(unsafe.Pointer(&rect))) != nil {
			return 0, errors.New("cannot move indicator away from target")
		}
		for _, m := range b.monitors {
			if Contains(m.Rect, p) {
				y := m.Rect.Y + 12
				if int(rect.Top) < m.Rect.Y+m.Rect.Height/2 {
					y = m.Rect.Y + m.Rect.Height - int(rect.Bottom-rect.Top) - 12
				}
				if err := callOK(user32.NewProc("SetWindowPos"), hwnd, ^uintptr(0), uintptr(rect.Left), uintptr(y), 0, 0, 0x0011); err != nil {
					return 0, errors.New("cannot move indicator away from target")
				}
				break
			}
		}
		hwnd = windowFromPoint(int32(p.X), int32(p.Y))
		hwnd, _, _ = user32.NewProc("GetAncestor").Call(hwnd, 2)
	}
	return hwnd, nil
}
func (b *nativeBackend) Hit(p protocol.Point) (protocol.DesktopWindow, error) {
	hwnd, err := b.uncoveredRoot(p)
	if err != nil {
		return protocol.DesktopWindow{}, err
	}
	return windowInfo(hwnd)
}

func windowHandle(w protocol.DesktopWindow) (uintptr, error) {
	n, err := strconv.ParseUint(w.ID, 16, 64)
	if err != nil {
		return 0, errors.New("invalid window identity")
	}
	current, err := windowInfo(uintptr(n))
	if err != nil || current.PID != w.PID {
		return 0, errors.New("selected window no longer exists")
	}
	return uintptr(n), nil
}
func (b *nativeBackend) Focus(w protocol.DesktopWindow) error {
	hwnd, err := windowHandle(w)
	if err != nil {
		return err
	}
	iconic, _, _ := user32.NewProc("IsIconic").Call(hwnd)
	if iconic != 0 {
		user32.NewProc("ShowWindowAsync").Call(hwnd, 9)
	} // SW_RESTORE
	user32.NewProc("SetForegroundWindow").Call(hwnd)
	return nil // Engine verifies, handles foreground lock, then verifies again.
}

type monitorEnumeration struct {
	snap      *protocol.DesktopSnapshot
	err       error
	rotations map[string]uint32
}

var monitorCallback = windows.NewCallback(func(hmon, dc uintptr, rect *winRect, state *monitorEnumeration) uintptr {
	var info monitorInfo
	info.Size = uint32(unsafe.Sizeof(info))
	if callOK(user32.NewProc("GetMonitorInfoW"), hmon, uintptr(unsafe.Pointer(&info))) != nil {
		state.err = errors.New("cannot read monitor geometry")
		return 0
	}
	r := info.Monitor.rect()
	name := windows.UTF16ToString(info.Device[:])
	// GetDpiForWindow is DPI-aware. A hidden, nonactivating probe on this
	// monitor gives its current effective DPI, including a custom scale.
	probe, _, _ := user32.NewProc("CreateWindowExW").Call(0x08000080, uintptr(unsafe.Pointer(utf16ptr("STATIC"))), 0, 0x80000000, uintptr(r.X), uintptr(r.Y), 1, 1, 0, 0, 0, 0)
	if probe == 0 {
		state.err = errors.New("cannot measure monitor DPI")
		return 0
	}
	dpi, _, _ := user32.NewProc("GetDpiForWindow").Call(probe)
	user32.NewProc("DestroyWindow").Call(probe)
	if dpi == 0 {
		state.err = errors.New("cannot measure monitor DPI")
		return 0
	}
	// Read only valid rotation from DEVMODEW, so 180-degree rotation is caught
	// without treating refresh-rate/driver changes as new pointer coordinates.
	var mode [220]byte
	binary.LittleEndian.PutUint16(mode[68:], uint16(len(mode)))
	if callOK(user32.NewProc("EnumDisplaySettingsW"), uintptr(unsafe.Pointer(&info.Device[0])), uintptr(0xffffffff), uintptr(unsafe.Pointer(&mode[0]))) != nil {
		state.err = errors.New("cannot read display mode")
		return 0
	}
	state.rotations[name] = displayRotation(mode)
	state.snap.Monitors = append(state.snap.Monitors, protocol.DesktopMonitor{ID: name, Rect: r, Primary: info.Flags&1 != 0, DPI: int(dpi)})
	return 1
})

type windowEnumeration struct {
	backend *nativeBackend
	result  []protocol.DesktopWindow
}

var windowCallback = windows.NewCallback(func(hwnd uintptr, state *windowEnumeration) uintptr {
	if hwnd == state.backend.banner.Load() {
		return 1
	}
	visible, _, _ := user32.NewProc("IsWindowVisible").Call(hwnd)
	iconic, _, _ := user32.NewProc("IsIconic").Call(hwnd)
	if visible == 0 || iconic != 0 {
		return 1
	}
	var cloaked uint32
	dwmapi.NewProc("DwmGetWindowAttribute").Call(hwnd, 14, uintptr(unsafe.Pointer(&cloaked)), 4)
	if cloaked != 0 {
		return 1
	}
	w, err := windowInfo(hwnd)
	if err == nil && w.PID != windows.GetCurrentProcessId() && w.Rect.Width > 0 && w.Rect.Height > 0 {
		state.result = append(state.result, w)
	}
	return 1
})
