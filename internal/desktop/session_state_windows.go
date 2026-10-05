//go:build windows

package desktop

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var wtsQuerySessionInformation = windows.NewLazySystemDLL("wtsapi32.dll").NewProc("WTSQuerySessionInformationW")

// Compile-time ABI check on every Windows target, including amd64 and arm64.
const wtsFlagsOffset = unsafe.Offsetof(wtsInfoExPrefix{}.Data) + unsafe.Offsetof(wtsInfoExPrefix{}.Data.SessionFlags)

var _ [16 - wtsFlagsOffset]byte
var _ [wtsFlagsOffset - 16]byte

func querySessionState(session uint32) int32 {
	if wtsQuerySessionInformation.Find() != nil {
		return wtsSessionUnknown
	}
	var info *wtsInfoExPrefix
	var size uint32
	const wtsSessionInfoEx = 25
	ok, _, _ := wtsQuerySessionInformation.Call(0, uintptr(session), wtsSessionInfoEx, uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&size)))
	if info != nil {
		defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(info)))
	}
	if ok == 0 {
		return wtsSessionUnknown
	}
	return wtsSessionFlags(info, size, session)
}

// CheckCaptureSession protects the old in-process capture without changing
// which sessions it supports. The helper retains its own console selection.
func CheckCaptureSession() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	session, err := processSession()
	if err != nil {
		return checkInputDesktop()
	}
	return checkSessionDesktop(session, querySessionState, checkInputDesktop)
}
