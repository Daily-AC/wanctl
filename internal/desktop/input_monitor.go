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
