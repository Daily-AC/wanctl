//go:build windows

package desktop

import (
	"errors"
	"strconv"
	"unsafe"

	"wanctl/internal/protocol"
)

type guiThreadInfo struct {
	Size, Flags                                        uint32
	Active, Focus, Capture, MenuOwner, MoveSize, Caret uintptr
	CaretRect                                          winRect
}

// UnicodeTo addresses the validated focused control, never the global input
// queue. A foreground check followed by SendInput has an unavoidable routing
// race: the foreground may change before Windows delivers the queued packet.
// An already-addressed WM_CHAR can at most reach the original control; it
// cannot be redirected into the window that stole the foreground.
func (b *nativeBackend) UnicodeTo(expected protocol.DesktopWindow, unit uint16, down bool) error {
	return addressedUnicode(expected, unit, down, b.physicalInputError, focusedTextTarget, sendUnicodeMessage)
}

func sendUnicodeMessage(target uintptr, unit uint16) error {
	// Bound waiting as well as the recipient. A hung UI must not hold up human
	// cancellation/key release; never retry an uncertain character delivery.
	var result uintptr
	sent, _, _ := user32.NewProc("SendMessageTimeoutW").Call(target, 0x0102, uintptr(unit), 1, 0x0023, 100, uintptr(unsafe.Pointer(&result)))
	if sent == 0 {
		return errors.New("state unknown: addressed Unicode delivery failed or timed out; do not replay")
	}
	return nil
}

func focusedTextTarget(expected protocol.DesktopWindow) (uintptr, error) {
	// Do not obtain the destination from another full windowInfo() snapshot:
	// a metadata snapshot cannot bind delivery. Check the live HWND/PID, then the
	// receiving control's ancestry/PID, without querying titles or process paths.
	parsed, err := strconv.ParseUint(expected.ID, 16, 64)
	if err != nil || parsed == 0 {
		return 0, errors.New("invalid text target window")
	}
	root := uintptr(parsed)
	changed := func() (uintptr, error) {
		return 0, errors.New("foreground window changed; stopped before sending input")
	}
	foreground, _, _ := user32.NewProc("GetForegroundWindow").Call()
	var pid uint32
	thread, _, _ := user32.NewProc("GetWindowThreadProcessId").Call(root, uintptr(unsafe.Pointer(&pid)))
	if foreground != root || thread == 0 || pid != expected.PID {
		return changed()
	}
	var gui guiThreadInfo
	gui.Size = uint32(unsafe.Sizeof(gui))
	if callOK(user32.NewProc("GetGUIThreadInfo"), thread, uintptr(unsafe.Pointer(&gui))) != nil || gui.Active != root || gui.Focus == 0 {
		return changed()
	}
	ancestor, _, _ := user32.NewProc("GetAncestor").Call(gui.Focus, 2) // GA_ROOT
	var owner uint32
	user32.NewProc("GetWindowThreadProcessId").Call(gui.Focus, uintptr(unsafe.Pointer(&owner)))
	foreground, _, _ = user32.NewProc("GetForegroundWindow").Call()
	if ancestor != root || owner != expected.PID || foreground != root {
		return changed()
	}
	return gui.Focus, nil
}
