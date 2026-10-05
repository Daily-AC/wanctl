//go:build windows && (amd64 || arm64)

package desktop

// POINT is passed by value in a single 64-bit argument on Win64.
func windowFromPoint(x, y int32) uintptr {
	h, _, _ := user32.NewProc("WindowFromPoint").Call(uintptr(uint64(uint32(x)) | uint64(uint32(y))<<32))
	return h
}
