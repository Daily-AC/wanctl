package desktop

const ownInputMarker = 0x57414e43

// Raw Input can also carry injected events. Classify source metadata, not the
// key/button payload: ignore our marker and explicitly injected/system input.
// A null device alone is NOT proof of injection: precision touchpads use it.
func rawDeviceInput(kind uint32, device uintptr, origin uint32, extra uintptr) bool {
	if kind > 1 || extra == ownInputMarker || origin == 2 || origin == 4 {
		return false
	}
	return device != 0 || origin == 1 || kind == 0
}
