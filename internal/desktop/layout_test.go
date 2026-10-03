package desktop

import (
	"encoding/binary"
	"testing"

	"wanctl/internal/protocol"
)

func TestDisplayLayoutIgnoresActivityButTracksCoordinateChanges(t *testing.T) {
	m := []protocol.DesktopMonitor{{ID: "display1", Rect: protocol.Rect{Width: 1920, Height: 1080}, DPI: 120, Primary: true}}
	var before, after [220]byte
	binary.LittleEndian.PutUint32(before[72:], 0x80)
	binary.LittleEndian.PutUint32(before[184:], 60) // refresh rate
	after = before
	binary.LittleEndian.PutUint32(after[184:], 120) // activity/DRR, not coordinates
	after[188] = 1
	after[204] = 255 // ICM and reserved/driver state
	old := layoutFingerprint(1, m, map[string]uint32{"display1": displayRotation(before)})
	if got := layoutFingerprint(1, m, map[string]uint32{"display1": displayRotation(after)}); got != old {
		t.Fatal("activity-only display state was reported as changed coordinates")
	}
	for _, tc := range []struct {
		name     string
		session  uint32
		monitor  protocol.DesktopMonitor
		rotation uint32
	}{
		{"DPI", 1, protocol.DesktopMonitor{ID: "display1", Rect: m[0].Rect, DPI: 144, Primary: true}, 0},
		{"negative origin", 1, protocol.DesktopMonitor{ID: "display1", Rect: protocol.Rect{X: -1920, Y: -200, Width: 1920, Height: 1080}, DPI: 120, Primary: true}, 0},
		{"resolution", 1, protocol.DesktopMonitor{ID: "display1", Rect: protocol.Rect{Width: 2560, Height: 1440}, DPI: 120, Primary: true}, 0},
		{"session", 2, m[0], 0}, {"rotation", 1, m[0], 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := layoutFingerprint(tc.session, []protocol.DesktopMonitor{tc.monitor}, map[string]uint32{"display1": tc.rotation}); got == old {
				t.Fatal("coordinate/session change went undetected")
			}
		})
	}
	after = before
	binary.LittleEndian.PutUint32(after[84:], 2)
	if displayRotation(after) != 2 {
		t.Fatal("rotation was lost")
	}
	binary.LittleEndian.PutUint32(after[72:], 0)
	if displayRotation(after) != 0 {
		t.Fatal("undefined DEVMODE union member was treated as rotation")
	}
}
func TestDisplayLayoutIsIndependentOfMonitorEnumerationOrder(t *testing.T) {
	m := example().Monitors
	m[0].ID = "left"
	m[1].ID = "right"
	first := layoutFingerprint(1, m, nil)
	if got := layoutFingerprint(1, []protocol.DesktopMonitor{m[1], m[0]}, nil); got != first {
		t.Fatal("enumeration order changed layout")
	}
	if got := layoutFingerprint(1, m[:1], nil); got == first {
		t.Fatal("monitor removal was not detected")
	}
}
