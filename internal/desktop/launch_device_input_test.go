package desktop

import (
	"context"
	"errors"
	"testing"

	"wanctl/internal/protocol"
)

func observeKeyboardFlags(s *Signal, flags uint32, extra uintptr) {
	s.InputEvent(1, flags&0x10 != 0, extra)
}

func observeRawDevice(s *Signal, kind uint32, device uintptr, origin uint32, extra uintptr) {
	if observer, ok := any(s).(interface {
		RawInputDeviceEvent(uint32, uintptr, uint32, uintptr)
	}); ok {
		observer.RawInputDeviceEvent(kind, device, origin, extra)
	} else {
		s.RawInputEvent(kind, origin, extra) // b1d5fef discards the device handle
	}
}

func TestLaunchDeviceLessKeyboardDoesNotVetoInjectedHook(t *testing.T) {
	for _, origin := range []uint32{0, 1} {
		sig := NewSignal()
		sig.launchWaiting.Store(true)
		// Candidate real shape: keybd_event has an injected low-level hook,
		// then a device-less raw keyboard notification with no injected origin.
		// Exact device metadata remains for the diagnostic build to measure.
		for range 2 { // Alt down/up, no key content retained
			observeKeyboardFlags(sig, 0x10, 0)
			observeRawDevice(sig, 1, 0, origin, 0)
		}
		if err := sig.Check(context.Background()); err != nil {
			t.Errorf("device-less keyboard origin=%d overrode injected hook during launch: %v", origin, err)
		}
	}
}

func TestLaunchDeviceClassificationPreservesStopBoundaries(t *testing.T) {
	for _, active := range []bool{false, true} {
		for _, tc := range []struct {
			kind   uint32
			device uintptr
			origin uint32
		}{
			{1, 23, 0}, {1, 23, 1}, // device-backed keyboard, including unknown origin
			{0, 0, 0}, {0, 0, 1}, {0, 0, 2}, {0, 0, 4}, // touchpad/mouse, even device-less
		} {
			sig := NewSignal()
			sig.launchWaiting.Store(active)
			observeRawDevice(sig, tc.kind, tc.device, tc.origin, 0)
			if !errors.Is(sig.Check(context.Background()), ErrHumanInput) {
				t.Errorf("foreign input ignored: launch=%v kind=%d device_zero=%v origin=%d", active, tc.kind, tc.device == 0, tc.origin)
			}
		}
		for _, flags := range []uint32{0, 1, 0x20, 0x80} { // no injection bits
			sig := NewSignal()
			sig.launchWaiting.Store(active)
			observeKeyboardFlags(sig, flags, 0)
			if !errors.Is(sig.Check(context.Background()), ErrHumanInput) {
				t.Fatal("un-injected keyboard hook was ignored")
			}
		}
		sig := NewSignal()
		sig.launchWaiting.Store(active)
		observeRawDevice(sig, 1, 0, 0, ownInputMarker)
		observeKeyboardFlags(sig, 0x12, ownInputMarker)
		if sig.Check(context.Background()) != nil {
			t.Fatal("own input stopped the batch")
		}
	}
	for _, origin := range []uint32{0, 1, 2, 4} {
		sig := NewSignal()
		observeRawDevice(sig, 1, 0, origin, 0)
		if !errors.Is(sig.Check(context.Background()), ErrHumanInput) {
			t.Fatal("device-less foreign input ignored outside launch")
		}
	}
}

type deviceKeyboardLauncher struct {
	*fakeBackend
	windows int
}

func (b *deviceKeyboardLauncher) Windows() ([]protocol.DesktopWindow, error) {
	b.windows++
	if b.windows == 1 {
		observeKeyboardFlags(b.signal, 0x10, 0)
		observeRawDevice(b.signal, 1, 0, 0, 0)
		observeKeyboardFlags(b.signal, 0x10, 0)
		observeRawDevice(b.signal, 1, 0, 0, 0)
		return nil, nil
	}
	return b.fakeBackend.Windows()
}

func TestLaunchCompletesWithDeviceLessStartupKeys(t *testing.T) {
	b := &deviceKeyboardLauncher{fakeBackend: fake()}
	res := (Engine{Backend: b}).Run(context.Background(), "owner", example(), []protocol.DesktopAction{{Type: "launch", Program: "launcher.exe"}})
	if res.Status != "completed" || res.Completed != 1 || b.held != 0 {
		t.Fatalf("launch with device-less startup keys: status=%s error=%q held=%d", res.Status, res.Error, b.held)
	}
}
