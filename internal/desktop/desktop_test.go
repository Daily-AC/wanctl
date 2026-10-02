package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"wanctl/internal/protocol"
)

func example() protocol.DesktopSnapshot {
	return protocol.DesktopSnapshot{Layout: "two-monitor-125", Source: protocol.Rect{X: -1920, Y: -200, Width: 3840, Height: 2160}, Width: 1280, Height: 720, Monitors: []protocol.DesktopMonitor{{Rect: protocol.Rect{X: -1920, Y: -200, Width: 1920, Height: 2160}, DPI: 120}, {Rect: protocol.Rect{X: 0, Y: -200, Width: 1920, Height: 2160}, DPI: 144}}, Foreground: protocol.DesktopWindow{ID: "42", PID: 7, ElevationKnown: true}, Windows: []protocol.DesktopWindow{{ID: "42", PID: 7, ElevationKnown: true, Rect: protocol.Rect{X: -1920, Y: -200, Width: 3840, Height: 2160}}}}
}
func TestMapCoordinates(t *testing.T) {
	s := example()
	for _, tc := range []struct{ x, y, px, py int }{{0, 0, -1920, -200}, {640, 100, 0, 100}, {1279, 719, 1917, 1957}} {
		p, err := MapPoint(s, s.Layout, tc.x, tc.y)
		if err != nil || p.X != tc.px || p.Y != tc.py {
			t.Fatalf("%+v => %+v %v", tc, p, err)
		}
	}
	for _, tc := range []struct{ x, y int }{{-1, 0}, {1280, 0}, {0, 720}} {
		if _, err := MapPoint(s, s.Layout, tc.x, tc.y); err == nil {
			t.Fatal("accepted outside image")
		}
	}
	if _, err := MapPoint(s, "changed dpi", 1, 1); err == nil {
		t.Fatal("accepted changed layout")
	}
	s.Monitors[0].Rect.Height = 100
	if _, err := MapPoint(s, s.Layout, 2, 200); err == nil {
		t.Fatal("accepted monitor gap")
	}
}
func TestCropAndRounding(t *testing.T) {
	s := example()
	r, err := CropSource(s, protocol.Rect{X: 600, Y: 20, Width: 100, Height: 200})
	if err != nil || r != (protocol.Rect{X: -120, Y: -140, Width: 300, Height: 600}) {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := CropSource(s, protocol.Rect{X: 1279, Width: 2, Height: 1}); err == nil {
		t.Fatal("bad region accepted")
	}
	s.Source.Width = 1920
	s.Width = 1280
	p, err := MapPoint(s, s.Layout, 1, 0)
	if err != nil || p.X != -1918 {
		t.Fatalf("half-pixel rounding: %v %v", p, err)
	}
}
func TestSnapshotsAreBoundedIsolatedExpiringAndSingleUse(t *testing.T) {
	var store Store
	now := time.Now()
	s, err := store.Put("a", example(), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Get("b", s.ID, true, now); err == nil {
		t.Fatal("other controller accepted")
	}
	if _, err = store.Get("a", s.ID, true, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Get("a", s.ID, true, now); err == nil || !strings.Contains(err.Error(), "state unknown") {
		t.Fatal("replayed consumed snapshot")
	}
	if _, err = store.Get("a", s.ID, false, now.Add(SnapshotTTL)); err == nil {
		t.Fatal("stale accepted")
	}
	var restarted Store
	if _, err = restarted.Get("a", s.ID, true, now); err == nil {
		t.Fatal("survived restart")
	}
	for i := 0; i < snapshotsPerController+1; i++ {
		_, _ = store.Put("a", example(), now)
	}
	if len(store.peers["a"]) != snapshotsPerController {
		t.Fatal("unbounded")
	}
	if _, err = store.Get("a", s.ID, true, now); err == nil {
		t.Fatal("eviction revived request")
	}
}
func TestPrivacySummaryAndValidation(t *testing.T) {
	secret := "SYNTHETIC_private_中文_🚀"
	req := &protocol.DesktopRequest{ScreenshotID: "s", Actions: []protocol.DesktopAction{{Type: "type", Text: secret}, {Type: "click", X: 2, Y: 7}, {Type: "key", Key: secret}, {Type: "launch", Program: secret, Args: []string{secret}}, {Type: secret}}}
	for _, coordinates := range []bool{false, true} {
		s := Summary("act", req, coordinates)
		if strings.Contains(s, secret) {
			t.Fatal("text leaked")
		}
		if coordinates && !strings.Contains(s, "characters") {
			t.Fatal("no count")
		}
	}
	if err := Validate("act", req); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("validation leaked text or accepted bad keys")
	}
	if _, err := ParseKeys("ctrl+l"); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseKeys("ctrl+ctrl"); err == nil {
		t.Fatal("duplicate modifier")
	}
}
func TestFocusGuard(t *testing.T) {
	a := example().Foreground
	if err := FocusGuard(a, a); err != nil {
		t.Fatal(err)
	}
	for _, b := range []protocol.DesktopWindow{{ID: "43", PID: 7, ElevationKnown: true}, {ID: "42", PID: 8, ElevationKnown: true}, {ID: "42", PID: 7, Elevated: true, ElevationKnown: true}, {ID: "42", PID: 7}} {
		if FocusGuard(a, b) == nil {
			t.Fatalf("accepted %+v", b)
		}
	}
}

type fakeBackend struct {
	mu                       sync.Mutex
	signal                   *Signal
	foreground               protocol.DesktopWindow
	begins                   int
	pressed, released, moves int
	held                     int
	beginHuman               bool
	onPress                  func()
	onCheck                  func()
	failPress                bool
}

func fake() *fakeBackend { return &fakeBackend{foreground: example().Foreground} }
func (f *fakeBackend) Begin(_ string, s *Signal) (func(), error) {
	f.signal = s
	f.begins++
	if f.beginHuman {
		s.HumanInput(false)
	}
	return func() {}, nil
}
func (f *fakeBackend) Check(string) error {
	if f.onCheck != nil {
		f.onCheck()
	}
	return nil
}
func (f *fakeBackend) Foreground() (protocol.DesktopWindow, error)        { return f.foreground, nil }
func (f *fakeBackend) Hit(protocol.Point) (protocol.DesktopWindow, error) { return f.foreground, nil }
func (f *fakeBackend) Move(protocol.Point) error                          { f.moves++; return nil }
func (f *fakeBackend) Button(_ string, down bool) error                   { return f.input(down) }
func (f *fakeBackend) Wheel(int) error                                    { return nil }
func (f *fakeBackend) Unicode(_ uint16, down bool) error                  { return f.input(down) }
func (f *fakeBackend) Key(_ uint16, down bool) error                      { return f.input(down) }
func (f *fakeBackend) input(down bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if down {
		f.pressed++
		f.held++
		if f.onPress != nil {
			f.onPress()
		}
		if f.failPress {
			return errors.New("partial OS delivery")
		}
	} else {
		f.released++
		f.held--
	}
	return nil
}
func (f *fakeBackend) Focus(w protocol.DesktopWindow) error          { f.foreground = w; return nil }
func (f *fakeBackend) Launch(protocol.DesktopAction) (uint32, error) { return 7, nil }
func (f *fakeBackend) Windows() ([]protocol.DesktopWindow, error)    { return example().Windows, nil }
func (f *fakeBackend) Capture(*protocol.Rect) (protocol.DesktopSnapshot, []byte, error) {
	return example(), nil, nil
}
func TestHumanStopBeforeDuringTypeWaitAndHeldKeys(t *testing.T) {
	for _, phase := range []string{"before", "type", "wait", "key", "drag", "stop-button"} {
		t.Run(phase, func(t *testing.T) {
			f := fake()
			sig := NewSignal()
			actions := []protocol.DesktopAction{{Type: "type", Text: "must never be fully sent"}, {Type: "click", X: 100, Y: 100}}
			switch phase {
			case "before":
				f.beginHuman = true
			case "type", "stop-button":
				f.onPress = func() { sig.HumanInput(false) }
			case "key":
				actions[0] = protocol.DesktopAction{Type: "key", Key: "ctrl+shift+l"}
				f.onPress = func() { sig.HumanInput(false) }
			case "drag":
				actions[0] = protocol.DesktopAction{Type: "drag", X: 100, Y: 100, ToX: 200, ToY: 200}
				f.onPress = func() { sig.HumanInput(false) }
			case "wait":
				actions[0] = protocol.DesktopAction{Type: "wait", Millis: 5000}
				f.onCheck = func() { sig.HumanInput(false) }
			}
			start := time.Now()
			res := (Engine{Backend: f, Signal: sig}).Run(context.Background(), "controller", example(), actions)
			if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
				t.Fatalf("stop took %v", elapsed)
			}
			if res.Status != "interrupted" || res.Error != protocol.DesktopHumanInput || res.Completed != 0 || f.held != 0 || f.pressed != f.released {
				t.Fatalf("res=%+v held=%d presses=%d release=%d", res, f.held, f.pressed, f.released)
			}
			if f.moves > 1 {
				t.Fatal("queued click ran")
			}
		})
	}
}
func TestSyntheticInputDoesNotStopAndBetweenCallsReset(t *testing.T) {
	s := NewSignal()
	s.HumanInput(true)
	if err := s.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.HumanInput(false)
	if !errors.Is(s.Check(context.Background()), ErrHumanInput) {
		t.Fatal("human input missed")
	}
	// There is no lease or cooldown. A NEW explicitly requested batch has its
	// own signal; the caller must ask the user rather than automatically retry.
	if err := NewSignal().Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestCancelAndPartialReleaseWithoutReplay(t *testing.T) {
	f := fake()
	f.failPress = true
	actions := []protocol.DesktopAction{{Type: "wait"}, {Type: "type", Text: "x"}, {Type: "click", X: 10, Y: 10}}
	res := (Engine{Backend: f}).Run(context.Background(), "controller", example(), actions)
	if res.Status != "partial" || res.Completed != 1 || res.FailedIndex != 1 || f.pressed != 1 || f.released != 1 || f.moves != 0 {
		t.Fatalf("%+v %+v", res, f)
	}
	f = fake()
	ctx, cancel := context.WithCancel(context.Background())
	f.onPress = cancel
	res = (Engine{Backend: f}).Run(ctx, "controller", example(), []protocol.DesktopAction{{Type: "key", Key: "ctrl+l"}})
	if res.Status != "partial" || f.held != 0 {
		t.Fatalf("%+v %+v", res, f)
	}
}
func TestFocusChangePreventsTypingAndCoveredClick(t *testing.T) {
	for _, a := range []protocol.DesktopAction{{Type: "type", Text: "SECRET"}, {Type: "key", Key: "enter"}, {Type: "click", X: 10, Y: 10}} {
		f := fake()
		f.foreground.ID = "other"
		res := (Engine{Backend: f}).Run(context.Background(), "controller", example(), []protocol.DesktopAction{a})
		if res.Error == "" || f.pressed != 0 || f.moves != 0 {
			t.Fatalf("%+v", res)
		}
		raw, _ := json.Marshal(res)
		if strings.Contains(string(raw), "SECRET") {
			t.Fatal("result leaked input")
		}
	}
}
