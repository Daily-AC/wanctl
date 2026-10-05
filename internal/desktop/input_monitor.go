package desktop

import (
	"crypto/rand"
	"encoding/binary"
)

// Each helper invocation is a fresh process. Keep the nonzero random marker
// within signed 32 bits as well as ULONG_PTR; never reuse a shared constant.
var ownInputMarker = func() uintptr {
	var b [4]byte
	for {
		rand.Read(b[:]) // Go's crypto/rand.Read terminates on entropy failure.
		if marker := uintptr(binary.LittleEndian.Uint32(b[:]) & 0x7fffffff); marker != 0 {
			return marker
		}
	}
}()

func isOwnInput(extra uintptr) bool { return extra == ownInputMarker }

// Only our marker exempts keyboard/mouse input, irrespective of source/device.
func rawDeviceInput(kind uint32, _ uintptr, _ uint32, extra uintptr) bool {
	return kind <= 1 && !isOwnInput(extra)
}

func (s *Signal) InputEvent(kind uint32, injected bool, extra uintptr) {
	if rawDeviceInput(kind, 0, 0, extra) && !(kind == 1 && injected && s.launchWaiting.Load()) {
		s.HumanInput(false)
	}
}

func (s *Signal) RawInputEvent(kind, origin uint32, extra uintptr) {
	// IMO_SYSTEM, like IMO_INJECTED, explicitly denotes synthetic input.
	// Hardware (including UIAccess-attributed input) and unknown origins stop.
	s.InputEvent(kind, origin == 2 || origin == 4, extra)
}
