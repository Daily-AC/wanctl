package desktop

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"wanctl/internal/protocol"
)

// Replay the native Raw Input adapter on the old build as well as the fixed
// build. IMO_SYSTEM (4) is explicitly system-injected, not hardware input.
// This is a documented source category, not a captured device packet.
func observeRawOrigin(sig *Signal, kind, origin uint32, extra uintptr) {
	if observer, ok := any(sig).(interface{ RawInputEvent(uint32, uint32, uintptr) }); ok {
		observer.RawInputEvent(kind, origin, extra)
	} else {
		sig.InputEvent(kind, origin == 2, extra)
	}
}

type launcherOrderBackend struct {
	*fakeBackend
	child        protocol.DesktopWindow
	appeared     bool
	scriptRan    bool
	order        []string
	stopKind     int
	stopAt       time.Time
	stopInjected bool
}

func (b *launcherOrderBackend) Launch(protocol.DesktopAction) (uint32, error) {
	b.order = append(b.order, "node starts", "port 8090 accepts", "Chrome starts")
	return 7, nil // PowerShell, NOT Chrome's PID
}
func (b *launcherOrderBackend) Windows() ([]protocol.DesktopWindow, error) {
	if !b.appeared {
		b.appeared = true
		b.order = append(b.order, "Chrome window appears")
	}
	return []protocol.DesktopWindow{b.child}, nil
}
func (b *launcherOrderBackend) Check(string) error {
	if b.appeared && !b.scriptRan {
		b.scriptRan = true
		for _, event := range []string{"synthetic Alt down", "synthetic Alt up"} {
			b.order = append(b.order, event)
			b.signal.InputEvent(1, true, 0)     // LLKHF_INJECTED
			observeRawOrigin(b.signal, 1, 4, 0) // system-injected Raw Input source
			if b.stopKind >= 0 && b.stopAt.IsZero() {
				b.stopAt = time.Now()
				b.signal.InputEvent(uint32(b.stopKind), b.stopInjected, 0)
			}
		}
		b.order = append(b.order, "launcher raises Chrome")
		b.foreground = b.child
	}
	return nil
}

func TestLaunchRealLauncherOrderWithAndWithoutTitle(t *testing.T) {
	for _, title := range []string{"路人甲", ""} {
		name := "matching-title"
		if title == "" {
			name = "no-title"
		}
		t.Run(name, func(t *testing.T) {
			b := &launcherOrderBackend{fakeBackend: fake(), child: protocol.DesktopWindow{ID: "chrome", PID: 8, Title: "路人甲", ElevationKnown: true}, stopKind: -1}
			a := protocol.DesktopAction{Type: "launch", Program: "launcher.exe", Title: title, TimeoutMS: 40}
			res := (Engine{Backend: b}).Run(context.Background(), "owner", example(), []protocol.DesktopAction{a})
			wantOrder := []string{"node starts", "port 8090 accepts", "Chrome starts", "Chrome window appears", "synthetic Alt down", "synthetic Alt up", "launcher raises Chrome"}
			if !reflect.DeepEqual(b.order, wantOrder) || b.held != 0 || res.Status == "interrupted" || strings.Contains(res.Error, protocol.DesktopHumanInput) {
				t.Fatalf("launcher interrupted: status=%s error=%q order=%v held=%d", res.Status, res.Error, b.order, b.held)
			}
			if title != "" {
				if res.Status != "completed" || !res.Actions[0].WindowAppeared || !res.Actions[0].Foreground {
					t.Fatalf("matched child did not complete launch: %+v", res)
				}
			} else if res.Status != "partial" || res.Actions[0].WindowAppeared || !strings.Contains(res.Error, "window did not appear before timeout") {
				t.Fatalf("PID mismatch must time out normally, without a human stop: %+v", res)
			}
			b.signal.InputEvent(1, true, 0)
			if !errors.Is(b.signal.Check(context.Background()), ErrHumanInput) {
				t.Fatal("synthetic keyboard exception remained after launch returned")
			}
			t.Logf("status=%s window_appeared=%v foreground=%v error=%q", res.Status, res.Actions[0].WindowAppeared, res.Actions[0].Foreground, res.Error)
		})
	}
}

func TestLaunchRawSystemKeyboardIsSynthetic(t *testing.T) {
	sig := NewSignal()
	sig.launchWaiting.Store(true)
	observeRawOrigin(sig, 1, 4, 0)
	if err := sig.Check(context.Background()); err != nil {
		t.Fatalf("IMO_SYSTEM keyboard interrupted active launch: %v", err)
	}
	sig.launchWaiting.Store(false)
	observeRawOrigin(sig, 1, 4, 0)
	if !errors.Is(sig.Check(context.Background()), ErrHumanInput) {
		t.Fatal("system-injected keyboard was ignored outside launch")
	}
}

func TestLaunchExceptionStillStopsHardwareUnknownAndMouseInput(t *testing.T) {
	for _, tc := range []struct {
		name         string
		kind, origin uint32
	}{
		{"hardware keyboard", 1, 1}, {"unknown keyboard", 1, 0},
		{"hardware mouse", 0, 1}, {"unknown mouse", 0, 0},
		{"injected mouse", 0, 2}, {"system-injected mouse", 0, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sig := NewSignal()
			sig.launchWaiting.Store(true)
			observeRawOrigin(sig, tc.kind, tc.origin, 0)
			if !errors.Is(sig.Check(context.Background()), ErrHumanInput) {
				t.Fatal("launch hid foreign hardware, unknown input or mouse input")
			}
		})
	}
	for _, tc := range []struct {
		kind     int
		injected bool
	}{{0, false}, {1, false}, {0, true}} {
		for _, title := range []string{"路人甲", ""} {
			b := &launcherOrderBackend{fakeBackend: fake(), child: protocol.DesktopWindow{ID: "chrome", PID: 8, Title: "路人甲", ElevationKnown: true}, stopKind: tc.kind, stopInjected: tc.injected}
			res := (Engine{Backend: b}).Run(context.Background(), "owner", example(), []protocol.DesktopAction{{Type: "launch", Program: "launcher.exe", Title: title, TimeoutMS: 10000}, {Type: "type", Text: "must not be sent"}})
			if b.stopAt.IsZero() || time.Since(b.stopAt) > 200*time.Millisecond || res.Status != "interrupted" || res.Error != protocol.DesktopHumanInput || res.Completed != 0 || b.pressed != 0 || b.held != 0 {
				t.Fatalf("stop did not retain its bounds: title=%q kind=%d injected=%v result=%+v held=%d", title, tc.kind, tc.injected, res, b.held)
			}
		}
	}
}
