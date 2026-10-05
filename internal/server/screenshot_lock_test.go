package server

import (
	"bytes"
	"context"
	"testing"

	"wanctl/internal/desktop"
	"wanctl/internal/protocol"
)

func TestLegacyScreenshotLockBeforeAndDuringCapture(t *testing.T) {
	const message = "desktop unavailable: screen locked or secure desktop active"
	for _, lockAt := range []int{1, 2, 0} {
		checks, captures := 0, 0
		original := []byte("unchanged legacy PNG")
		data, err := captureScreenChecked(context.Background(), func() error {
			checks++
			if checks == lockAt {
				return desktop.ErrLocked
			}
			return nil
		}, func(context.Context) ([]byte, error) { captures++; return original, nil })
		if lockAt == 0 {
			if err != nil || !bytes.Equal(data, original) || checks != 2 || captures != 1 {
				t.Fatalf("unlocked legacy capture changed: %q %v", data, err)
			}
		} else if err == nil || err.Error() != message || len(data) != 0 || captures != lockAt-1 {
			t.Fatalf("lockAt=%d captures=%d bytes=%d error=%v", lockAt, captures, len(data), err)
		}
	}
}

func TestLegacyHelperLockPreservesExactErrorAndNoImage(t *testing.T) {
	data, err := captureScreenUsing(context.Background(), func() (bool, error) { return true, nil }, func(context.Context, desktop.Job) (protocol.DesktopResult, []byte, error) {
		return protocol.DesktopResult{Status: "rejected", Error: "desktop unavailable: screen locked or secure desktop active"}, nil, nil
	}, func(context.Context) ([]byte, error) {
		t.Fatal("locked helper fell back to local capture")
		return nil, nil
	})
	if err == nil || err.Error() != "desktop unavailable: screen locked or secure desktop active" || len(data) != 0 {
		t.Fatalf("data=%q err=%v", data, err)
	}
}
