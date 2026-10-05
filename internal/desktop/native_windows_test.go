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

func TestShellExecuteInfoLayout(t *testing.T) {
	size, processOffset := uintptr(112), uintptr(104)
	if unsafe.Sizeof(uintptr(0)) == 4 {
		size, processOffset = 60, 56
	}
	if unsafe.Sizeof(shellExecuteInfo{}) != size || unsafe.Offsetof(shellExecuteInfo{}.Process) != processOffset {
		t.Fatal("SHELLEXECUTEINFOW ABI mismatch")
	}
}

func TestWin32RawInputMetadataLayouts(t *testing.T) {
	headerSize, deviceSize := uintptr(24), uintptr(16)
	if unsafe.Sizeof(uintptr(0)) == 4 {
		headerSize, deviceSize = 16, 12
	}
	if unsafe.Sizeof(rawInputHeader{}) != headerSize || unsafe.Sizeof(rawInputDevice{}) != deviceSize || unsafe.Sizeof(inputMessageSource{}) != 8 {
		t.Fatal("Raw Input metadata ABI mismatch")
	}
	if unsafe.Offsetof(rawInputHeader{}.Device) != 8 || unsafe.Offsetof(rawInputDevice{}.Target) != 8 {
		t.Fatal("Raw Input pointer alignment mismatch")
	}
}
