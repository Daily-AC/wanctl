package desktop

import (
	"context"
	"strings"
	"testing"

	"wanctl/internal/protocol"
)

// Foreground() snapshots A, then the OS switches to B while its title/process
// metadata is being read. It returns the captured A exactly like windowInfo
// can. The old global Unicode sender then delivers to B, and the NEXT guard
// reports the focus change, matching S24's one-character leak.
type stealingTextBackend struct {
	*fakeBackend
	switchAfter     int
	original, other int
	switched        bool
	heldText        int
}

func (b *stealingTextBackend) Foreground() (protocol.DesktopWindow, error) {
	observed := b.foreground
	if !b.switched && b.original == b.switchAfter {
		b.foreground = protocol.DesktopWindow{ID: "stealing-textbox", PID: 99, ElevationKnown: true}
		b.switched = true
	}
	return observed, nil
}

// Retain the old interface so this regression can run unchanged on db011ab.
func (b *stealingTextBackend) Unicode(_ uint16, down bool) error {
	if down {
		b.heldText++
		if b.switched {
			b.other++
		} else {
			b.original++
		}
	} else if b.heldText > 0 {
		b.heldText--
	}
	return nil
}

// The guarded path still uses the same global sender. Model its final live
// foreground check after metadata/monitoring, immediately before injection.
// db011ab does not call this optional guard, so this test still compiles there
// and reproduces the one-character leak via Unicode above.
func (b *stealingTextBackend) UnicodeChecked(expected protocol.DesktopWindow, unit uint16, down bool) error {
	if down {
		if err := FocusGuard(expected, b.foreground); err != nil {
			return err
		}
	}
	return b.Unicode(unit, down)
}
func TestTypeDoesNotDeliverAfterForegroundSwitch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after int
	}{{"before-first-character", 0}, {"mid-type", 247}} {
		t.Run(tc.name, func(t *testing.T) {
			b := &stealingTextBackend{fakeBackend: fake(), switchAfter: tc.after}
			result := (Engine{Backend: b}).Run(context.Background(), "owner", example(), []protocol.DesktopAction{{Type: "type", Text: strings.Repeat("x", 8000)}, {Type: "click", X: 64, Y: 80}})
			if b.other != 0 || b.original != tc.after {
				t.Fatalf("foreground switched after %d characters: original=%d stealing_window=%d; want zero delivery after switch (result=%q)", tc.after, b.original, b.other, result.Error)
			}
			if !b.switched || result.Status != "partial" || !strings.Contains(result.Error, "foreground window changed") || result.Completed != 0 || result.FailedIndex != 0 || len(result.Actions) != 1 || b.heldText != 0 || b.moves != 0 {
				t.Fatalf("incorrect stop/cleanup: %+v held=%d moves=%d", result, b.heldText, b.moves)
			}
			t.Logf("original=%d stealing_window=%d held=%d queued_clicks=%d", b.original, b.other, b.heldText, b.moves)
		})
	}
}
