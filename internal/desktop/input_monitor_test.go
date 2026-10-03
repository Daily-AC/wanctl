package desktop

import (
	"context"
	"errors"
	"testing"
)

func TestInputSourceClassificationDoesNotConfuseInjectionAndTouchpad(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kind   uint32
		device uintptr
		origin uint32
		extra  uintptr
		human  bool
	}{
		{"physical mouse", 0, 17, 1, 0, true},
		{"physical keyboard", 1, 19, 1, 0, true},
		{"precision touchpad without device handle", 0, 0, 1, 0, true},
		{"touchpad without source annotation", 0, 0, 0, 0, true},
		{"own injected mouse", 0, 0, 2, ownInputMarker, false},
		{"own injection without source annotation", 0, 0, 0, ownInputMarker, false},
		{"own injection with hardware annotation", 0, 17, 1, ownInputMarker, false},
		{"own injected Unicode", 1, 0, 2, ownInputMarker, false},
		{"unattributed software keyboard packet", 1, 0, 0, 0, false},
		{"other injected keyboard", 1, 0, 2, 0, false},
		{"system cursor reposition", 0, 0, 4, 0, false},
		{"unrelated HID", 2, 17, 1, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rawDeviceInput(tc.kind, tc.device, tc.origin, tc.extra); got != tc.human {
				t.Fatalf("human=%v, want %v", got, tc.human)
			}
		})
	}
}
func TestFailedPhysicalMonitorStopsInputWithoutHidingAHumanStop(t *testing.T) {
	sig := NewSignal()
	sig.failMonitor()
	if err := sig.Check(context.Background()); !errors.Is(err, errInputMonitor) {
		t.Fatal(err)
	}
	select {
	case <-sig.done:
	default:
		t.Fatal("monitor failure did not wake held-input release")
	}
	sig.HumanInput(false)
	if err := sig.Check(context.Background()); !errors.Is(err, ErrHumanInput) {
		t.Fatalf("monitor failure masked real input: %v", err)
	}
}
