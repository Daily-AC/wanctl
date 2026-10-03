package desktop

import (
	"context"
	"errors"
	"testing"

	"wanctl/internal/protocol"
)

func TestAddressedUnicodeCannotFollowAForegroundSwitchAtDelivery(t *testing.T) {
	const original, stealer = uintptr(10), uintptr(20)
	foreground := original
	received := map[uintptr]int{}
	err := addressedUnicode(example().Foreground, 'x', true, func() error { return nil }, func(protocol.DesktopWindow) (uintptr, error) { return original, nil }, func(target uintptr, unit uint16) error {
		// Switch AFTER the last check, at the OS delivery boundary. A global
		// sender would route by foreground here; the real WM_CHAR call takes the
		// captured HWND instead. Its one in-flight character can only reach A.
		foreground = stealer
		received[target]++
		return nil
	})
	if err != nil || foreground != stealer || received[stealer] != 0 || received[original] != 1 {
		t.Fatalf("delivery was redirected: %+v, %v", received, err)
	}
}

func TestAddressedUnicodeHonoursHumanStopBeforeSending(t *testing.T) {
	sig := NewSignal()
	sent := 0
	err := addressedUnicode(example().Foreground, 'x', true, func() error { return sig.Check(context.Background()) }, func(protocol.DesktopWindow) (uintptr, error) { sig.HumanInput(false); return 10, nil }, func(uintptr, uint16) error { sent++; return nil })
	if !errors.Is(err, ErrHumanInput) || sent != 0 {
		t.Fatalf("sent=%d err=%v", sent, err)
	}
	// Cleanup must neither send another character nor depend on current focus.
	err = addressedUnicode(example().Foreground, 'x', false, func() error { t.Fatal("cleanup queried input state"); return nil }, func(protocol.DesktopWindow) (uintptr, error) { t.Fatal("cleanup rebound focus"); return 0, nil }, func(uintptr, uint16) error { t.Fatal("cleanup sent text"); return nil })
	if err != nil {
		t.Fatal(err)
	}
}

func TestAddressedUnicodeNeverRetriesAnUncertainDelivery(t *testing.T) {
	calls := 0
	uncertain := errors.New("state unknown: delivery timed out")
	err := addressedUnicode(example().Foreground, 'x', true, func() error { return nil }, func(protocol.DesktopWindow) (uintptr, error) { return 10, nil }, func(uintptr, uint16) error { calls++; return uncertain })
	if calls != 1 || !errors.Is(err, uncertain) {
		t.Fatalf("delivery retried: calls=%d err=%v", calls, err)
	}
}
