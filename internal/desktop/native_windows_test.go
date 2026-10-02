//go:build windows

package desktop

import (
	"testing"
	"unsafe"
)

// No Win32 calls: these catch ABI/layout errors even in a Windows test binary
// intended for a later run with desktop interaction disabled.
func TestWin32InputAndCaptureLayouts(t *testing.T) {
	expectedInput, expectedMouse := uintptr(40), uintptr(32)
	if unsafe.Sizeof(uintptr(0)) == 4 {
		expectedInput, expectedMouse = 28, 24
	}
	if unsafe.Sizeof(winInput{}) != expectedInput || unsafe.Sizeof(mouseInput{}) != expectedMouse {
		t.Fatal("SendInput ABI size mismatch")
	}
	if unsafe.Sizeof(bitmapInfoHeader{}) != 40 {
		t.Fatal("BITMAPINFOHEADER ABI mismatch")
	}
	if unsafe.Sizeof(monitorInfo{}) != 104 {
		t.Fatal("MONITORINFOEXW ABI mismatch")
	}
	if unsafe.Offsetof(keyboardHook{}.Flags) != 8 || unsafe.Offsetof(mouseHook{}.Flags) != 12 {
		t.Fatal("hook flags offsets do not match Win32 ABI")
	}
}
