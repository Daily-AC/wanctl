//go:build windows && 386

package desktop

func windowFromPoint(x, y int32) uintptr {
	h, _, _ := user32.NewProc("WindowFromPoint").Call(uintptr(x), uintptr(y))
	return h
}
