package desktop

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"sort"

	"wanctl/internal/protocol"
)

// Only fields which determine screenshot coordinates belong in the layout
// identity. DEVMODE's refresh rate, color settings, driver-private/reserved
// bytes and unused union members can change without a layout change (including
// on user activity). Hashing that whole structure confuses input with layout.
func layoutFingerprint(session uint32, monitors []protocol.DesktopMonitor, rotations map[string]uint32) string {
	type monitor struct {
		protocol.DesktopMonitor
		Rotation uint32
	}
	ordered := make([]monitor, 0, len(monitors))
	for _, m := range monitors {
		ordered = append(ordered, monitor{m, rotations[m.ID]})
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	raw, _ := json.Marshal(struct {
		Session  uint32
		Monitors []monitor
	}{session, ordered})
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

// DEVMODEW: dmFields at 72, dmDisplayOrientation at 84. All other bytes are
// intentionally ignored; the monitor rectangle and GetDpiForWindow supply
// the remaining coordinate geometry. An unspecified union field is not data.
func displayRotation(mode [220]byte) uint32 {
	const dmDisplayOrientation = 0x80
	if binary.LittleEndian.Uint32(mode[72:])&dmDisplayOrientation == 0 {
		return 0
	}
	return binary.LittleEndian.Uint32(mode[84:])
}
