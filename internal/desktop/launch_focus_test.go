package desktop

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"wanctl/internal/protocol"
)

type elevatedForegroundBackend struct {
	*fakeBackend
	focusCalls, elevatedKeys int
}

func (b *elevatedForegroundBackend) Focus(w protocol.DesktopWindow) error {
	b.focusCalls++
	if b.focusCalls == 2 {
		b.foreground = w // activation succeeds on retry, without typing into A
	}
	return nil
}
func (b *elevatedForegroundBackend) Key(k uint16, down bool) error {
	if b.foreground.Elevated {
		if down {
			b.elevatedKeys++
			return errors.New("UIPI rejected input to elevated foreground")
		}
		return nil
	}
	return b.fakeBackend.Key(k, down)
}

func TestFocusLeavesElevatedForegroundWithoutInjectingIntoIt(t *testing.T) {
	b := &elevatedForegroundBackend{fakeBackend: fake()}
	b.foreground.Elevated = true
	snapshot := example()
	snapshot.Foreground = b.foreground
	target := protocol.DesktopWindow{ID: "normal-target", PID: 8, ElevationKnown: true}
	snapshot.Windows = append(snapshot.Windows, target)
	res := (Engine{Backend: b}).Run(context.Background(), "owner", snapshot, []protocol.DesktopAction{{Type: "focus", PID: 8}, {Type: "type", Text: "x"}})
	if res.Status != "completed" || res.Completed != 2 || b.foreground.ID != target.ID || b.elevatedKeys != 0 || b.pressed != 1 || b.held != 0 {
		t.Fatalf("focus from elevated foreground: status=%s error=%q focus_calls=%d elevated_keys=%d typed=%d held=%d", res.Status, res.Error, b.focusCalls, b.elevatedKeys, b.pressed, b.held)
	}
}

type shortcutBackend struct {
	*fakeBackend
	shellAction protocol.DesktopAction
	directCalls int
}

func (b *shortcutBackend) Launch(protocol.DesktopAction) (uint32, error) {
	b.directCalls++
	return 0, errors.New("CreateProcess cannot execute a shortcut")
}
func (b *shortcutBackend) LaunchShortcut(a protocol.DesktopAction) (uint32, error) {
	b.shellAction = a // shell receives the link itself, retaining its stored settings
	return 7, nil
}

func TestLaunchShortcutUsesShellWithoutReplacingStoredSettings(t *testing.T) {
	b := &shortcutBackend{fakeBackend: fake()}
	a := protocol.DesktopAction{Type: "launch", Program: `C:\Users\owner\Desktop\游戏.LNK`}
	res := (Engine{Backend: b}).Run(context.Background(), "owner", example(), []protocol.DesktopAction{a})
	if res.Status != "completed" || res.Completed != 1 || b.directCalls != 0 || !reflect.DeepEqual(b.shellAction, a) || !res.Actions[0].WindowAppeared || !res.Actions[0].Foreground {
		t.Fatalf("shortcut launch: status=%s error=%q direct_calls=%d shell_called=%v", res.Status, res.Error, b.directCalls, b.shellAction.Program != "")
	}
}

// Model the existing hook on the baseline and the shared phase-aware observer
// after the fix, keeping this regression source compilable on 6ac6ed0.
func observeStartupInput(sig *Signal, kind uint32, injected bool) {
	if observer, ok := any(sig).(interface{ InputEvent(uint32, bool, uintptr) }); ok {
		observer.InputEvent(kind, injected, 0)
	} else {
		sig.HumanInput(false) // 6ac6ed0: every event without our marker is foreign
	}
}

type startupInputBackend struct {
	*fakeBackend
	kind                 uint32
	injected, afterFocus bool
	launchError          bool
	windowCalls          int
	focused              bool
	at                   time.Time
}

func (b *startupInputBackend) Launch(protocol.DesktopAction) (uint32, error) {
	if b.launchError {
		return 0, errors.New("launch failed")
	}
	return 7, nil
}
func (b *startupInputBackend) Windows() ([]protocol.DesktopWindow, error) {
	b.windowCalls++
	if b.windowCalls == 1 && !b.afterFocus {
		b.at = time.Now()
		observeStartupInput(b.signal, b.kind, b.injected)
		return nil, nil
	}
	return b.fakeBackend.Windows()
}
func (b *startupInputBackend) Focus(w protocol.DesktopWindow) error {
	b.focused = true
	return b.fakeBackend.Focus(w)
}
func (b *startupInputBackend) Check(string) error {
	if b.afterFocus && b.focused && b.at.IsZero() {
		b.at = time.Now()
		observeStartupInput(b.signal, b.kind, b.injected)
	}
	return nil
}

func TestLaunchStartupKeysNeverHideHardwareOrLaterInput(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		kind                 uint32
		injected, afterFocus bool
		want                 string
		completed            int
	}{
		{"startup synthetic keyboard", 1, true, false, "completed", 2},
		{"hardware keyboard during wait", 1, false, false, "interrupted", 0},
		{"hardware mouse during wait", 0, false, false, "interrupted", 0},
		{"injected mouse during wait", 0, true, false, "interrupted", 0},
		{"synthetic keyboard after focus", 1, true, true, "interrupted", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &startupInputBackend{fakeBackend: fake(), kind: tc.kind, injected: tc.injected, afterFocus: tc.afterFocus}
			res := (Engine{Backend: b}).Run(context.Background(), "owner", example(), []protocol.DesktopAction{{Type: "launch", Program: "game.exe"}, {Type: "type", Text: "x"}})
			if res.Status != tc.want || res.Completed != tc.completed || b.held != 0 {
				t.Fatalf("status=%s completed=%d error=%q held=%d", res.Status, res.Completed, res.Error, b.held)
			}
			if tc.want == "interrupted" && (res.Error != protocol.DesktopHumanInput || b.at.IsZero() || time.Since(b.at) > 200*time.Millisecond || b.pressed != 0) {
				t.Fatalf("input did not stop immediately: %+v presses=%d", res, b.pressed)
			}
			observeStartupInput(b.signal, 1, true)
			if !errors.Is(b.signal.Check(context.Background()), ErrHumanInput) {
				t.Fatal("launch exception survived the action")
			}
		})
	}
	t.Run("launch failure clears exception", func(t *testing.T) {
		b := &startupInputBackend{fakeBackend: fake(), launchError: true}
		(Engine{Backend: b}).Run(context.Background(), "owner", example(), []protocol.DesktopAction{{Type: "launch", Program: "missing.exe"}})
		observeStartupInput(b.signal, 1, true)
		if !errors.Is(b.signal.Check(context.Background()), ErrHumanInput) {
			t.Fatal("failed launch retained input exception")
		}
	})
}
