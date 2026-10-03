package desktop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"

	"wanctl/internal/protocol"
)

var ErrHumanInput = errors.New(protocol.DesktopHumanInput)
var errInputMonitor = errors.New("desktop physical input monitor unavailable; input stopped")

// Signal remembers only whether human input occurred. Neither the hook nor
// this state machine retains keys, pointer positions or input content.
type Signal struct {
	human         atomic.Bool
	monitorFailed atomic.Bool
	once          sync.Once
	done          chan struct{}
}

func NewSignal() *Signal { return &Signal{done: make(chan struct{})} }
func (s *Signal) HumanInput(synthetic bool) {
	if !synthetic {
		s.human.Store(true)
		s.once.Do(func() { close(s.done) })
	}
}
func (s *Signal) failMonitor() {
	s.monitorFailed.Store(true)
	s.once.Do(func() { close(s.done) })
}

func (s *Signal) Check(ctx context.Context) error {
	if s.human.Load() {
		return ErrHumanInput
	}
	if s.monitorFailed.Load() {
		return errInputMonitor
	}
	return ctx.Err()
}

// Backend is implemented by the per-call Windows helper, and by test fakes.
// Input methods must be short. Long waits belong in Engine, with checkpoints.
type Backend interface {
	Begin(controller string, signal *Signal) (func(), error)
	Check(layout string) error
	Foreground() (protocol.DesktopWindow, error)
	Hit(protocol.Point) (protocol.DesktopWindow, error)
	Move(protocol.Point) error
	Button(string, bool) error
	Wheel(int) error
	Unicode(uint16, bool) error
	Key(uint16, bool) error
	Focus(protocol.DesktopWindow) error
	Launch(protocol.DesktopAction) (uint32, error)
	Windows() ([]protocol.DesktopWindow, error)
	Capture(*protocol.Rect) (protocol.DesktopSnapshot, []byte, error)
}

type held struct {
	button  string
	key     uint16
	unicode bool
}
type inputState struct {
	mu         sync.Mutex
	backend    Backend
	held       []held
	releaseErr error
}

func (s *inputState) send(h held, down bool) error {
	if h.button != "" {
		return s.backend.Button(h.button, down)
	}
	if h.unicode {
		return s.backend.Unicode(h.key, down)
	}
	return s.backend.Key(h.key, down)
}
func (s *inputState) press(ctx context.Context, sig *Signal, h held) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := sig.Check(ctx); err != nil {
		return err
	}
	// Register before SendInput: even a failure can mean a partial OS delivery.
	s.held = append(s.held, h)
	return s.send(h, true)
}
func (s *inputState) release() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var remaining []held
	for i := len(s.held) - 1; i >= 0; i-- {
		if err := s.send(s.held[i], false); err != nil {
			s.releaseErr = errors.New("state unknown: could not release held input")
			remaining = append(remaining, s.held[i])
		}
	}
	s.held = remaining
	if len(remaining) == 0 {
		s.releaseErr = nil
	}
	return s.releaseErr
}

// Physical input monitoring is independent of the banner/hook message loop.
// The monitor retains only an occurrence latch, never keys or input content.
// Portable backends need no native monitor; tests can supply a device feed.
type physicalInputMonitor interface {
	WatchInput(*Signal) (stop func(), err error)
}

type Engine struct {
	Backend Backend
	Signal  *Signal
}

// Run reports input delivery, never application success. A failed action may
// have emitted some input; only earlier, fully delivered actions are counted.
func (e Engine) Run(ctx context.Context, controller string, snapshot protocol.DesktopSnapshot, actions []protocol.DesktopAction) (res protocol.DesktopResult) {
	res.Status = "rejected"
	res.FailedIndex = -1
	sig := e.Signal
	if sig == nil {
		sig = NewSignal()
	}
	b := e.Backend
	var stopInput func()
	// Join/drain the device monitor before choosing the final reason, including
	// input that arrived while a display/focus query or cleanup was finishing.
	defer func() {
		if stopInput != nil {
			stopInput()
		}
		if sig.human.Load() {
			if res.Status != "unknown" {
				res.Status = "interrupted"
			}
			res.Error = protocol.DesktopHumanInput
			for i := range res.Actions {
				if res.Actions[i].Index == res.FailedIndex {
					res.Actions[i].Error = protocol.DesktopHumanInput
				}
			}
		}
	}()
	if monitor, ok := b.(physicalInputMonitor); ok {
		var err error
		stopInput, err = monitor.WatchInput(sig)
		if err != nil {
			res.Error = "desktop physical input monitor unavailable: " + err.Error()
			return
		}
	}
	clean, err := b.Begin(controller, sig)
	if err != nil {
		res.Error = "desktop safety monitor unavailable: " + err.Error()
		if sig.human.Load() {
			res.Status = "interrupted"
			res.Warning = res.Error
			res.Error = protocol.DesktopHumanInput
		}
		return
	}
	defer clean()
	input := &inputState{backend: b}
	// Releasing on a separate goroutine also covers a slow process launch or a
	// hung foreground API. No hook callback waits for SendInput or an engine lock.
	finished := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-sig.done:
			_ = input.release()
		case <-ctx.Done():
			_ = input.release()
		case <-finished:
		}
	}()
	defer func() {
		close(finished)
		<-watchDone
		if err := input.release(); err != nil {
			res.Status = "unknown"
			res.Warning = err.Error()
			res.Error = err.Error()
		}
	}()
	expected := snapshot.Foreground
	checkpoint := func() error {
		if err := sig.Check(ctx); err != nil {
			return err
		}
		err := b.Check(snapshot.Layout)
		if stopped := sig.Check(ctx); stopped != nil {
			return stopped
		}
		return err
	}
	wait := func(d time.Duration) error {
		deadline := time.Now().Add(d)
		for {
			if err := checkpoint(); err != nil {
				return err
			}
			left := time.Until(deadline)
			if left <= 0 {
				return nil
			}
			timer := time.NewTimer(min(left, 20*time.Millisecond))
			select {
			case <-timer.C:
			case <-sig.done:
				timer.Stop()
			case <-ctx.Done():
				timer.Stop()
			}
		}
	}
	foreground := func() error {
		if err := checkpoint(); err != nil {
			return err
		}
		w, err := b.Foreground()
		if stopped := sig.Check(ctx); stopped != nil {
			return stopped
		}
		if err != nil {
			return err
		}
		return FocusGuard(expected, w)
	}
	target := func(x, y int) (protocol.Point, error) {
		p, err := MapPoint(snapshot, snapshot.Layout, x, y)
		if err != nil {
			return p, err
		}
		want, err := TargetAt(snapshot, p)
		if err != nil {
			return p, err
		}
		got, err := b.Hit(p)
		if err != nil {
			return p, err
		}
		if err = FocusGuard(want, got); err != nil {
			return p, errors.New("target window changed, covered or elevated; take a fresh screenshot")
		}
		return p, nil
	}
	fail := func(i int, err error) {
		res.Status = "partial"
		res.FailedIndex = i
		res.Error = err.Error()
		if errors.Is(err, ErrHumanInput) {
			res.Status = "interrupted"
			res.Error = protocol.DesktopHumanInput
		}
	}
	if err = checkpoint(); err != nil {
		fail(0, err)
		return
	}
	for i, a := range actions {
		ar := protocol.DesktopActionResult{Index: i, Type: a.Type, Status: "input_sent"}
		err = checkpoint()
		if err == nil {
			switch a.Type {
			case "wait":
				err = wait(time.Duration(a.Millis) * time.Millisecond)
				ar.Status = "completed"
			case "type":
				for _, r := range a.Text {
					if err = foreground(); err != nil {
						break
					}
					for _, unit := range utf16.Encode([]rune{r}) {
						if err = input.press(ctx, sig, held{key: unit, unicode: true}); err != nil {
							break
						}
						if err = input.release(); err != nil {
							break
						}
					}
					if err != nil {
						break
					}
				}
			case "key":
				var keys []uint16
				keys, err = ParseKeys(a.Key)
				for _, k := range keys {
					if err != nil {
						break
					}
					if err = foreground(); err != nil {
						break
					}
					err = input.press(ctx, sig, held{key: k})
				}
				if err == nil {
					err = wait(30 * time.Millisecond)
				}
				if releaseErr := input.release(); releaseErr != nil {
					err = releaseErr
				}
			case "click", "scroll", "drag":
				var p protocol.Point
				p, err = target(a.X, a.Y)
				if err == nil {
					err = b.Move(p)
				}
				button := a.Button
				if button == "" {
					button = "left"
				}
				if err == nil && a.Type == "scroll" {
					if err = checkpoint(); err == nil {
						err = b.Wheel(a.Delta)
					}
				}
				if err == nil && a.Type == "click" {
					for n := 0; n < max(1, a.Count); n++ {
						if err = checkpoint(); err != nil {
							break
						}
						if _, err = target(a.X, a.Y); err != nil {
							break
						}
						if err = input.press(ctx, sig, held{button: button}); err != nil {
							break
						}
						if err = wait(20 * time.Millisecond); err != nil {
							break
						}
						if err = input.release(); err != nil {
							break
						}
						if n+1 < max(1, a.Count) {
							err = wait(50 * time.Millisecond)
							if err != nil {
								break
							}
						}
					}
				}
				if err == nil && a.Type == "drag" {
					var dest protocol.Point
					dest, err = MapPoint(snapshot, snapshot.Layout, a.ToX, a.ToY)
					if err == nil {
						err = input.press(ctx, sig, held{button: button})
					}
					duration := a.Millis
					if duration == 0 {
						duration = 300
					}
					steps := max(1, duration/10)
					for n := 1; err == nil && n <= steps; n++ {
						if err = checkpoint(); err != nil {
							break
						}
						err = b.Move(protocol.Point{X: p.X + (dest.X-p.X)*n/steps, Y: p.Y + (dest.Y-p.Y)*n/steps})
						if err == nil {
							err = wait(time.Duration(duration/steps) * time.Millisecond)
						}
					}
					if releaseErr := input.release(); releaseErr != nil {
						err = releaseErr
					}
				}
			case "focus":
				var matches []protocol.DesktopWindow
				for _, w := range snapshot.Windows {
					if (a.PID != 0 && w.PID == a.PID) || (a.Title != "" && containsTitle(w.Title, a.Title)) {
						matches = append(matches, w)
					}
				}
				if len(matches) != 1 {
					err = errors.New("focus must identify exactly one window in the screenshot")
					break
				}
				expected, err = focusWindow(ctx, sig, b, input, matches[0], wait, checkpoint)
				ar.Foreground = err == nil
				ar.Status = "completed"
			case "launch":
				ar.PID, err = b.Launch(a)
				if err != nil {
					err = errors.New("program could not be started")
					break
				}
				ar.Status = "partial"
				timeout := a.TimeoutMS
				if timeout == 0 {
					timeout = 10000
				}
				deadline := time.Now().Add(time.Duration(timeout) * time.Millisecond)
				for err == nil {
					if err = checkpoint(); err != nil {
						break
					}
					var windows []protocol.DesktopWindow
					windows, err = b.Windows()
					if err != nil {
						break
					}
					var matches []protocol.DesktopWindow
					for _, w := range windows {
						if (a.Title != "" && containsTitle(w.Title, a.Title)) || (a.Title == "" && w.PID == ar.PID) {
							matches = append(matches, w)
						}
					}
					if len(matches) > 1 {
						err = errors.New("program started but window selector is ambiguous")
						break
					}
					if len(matches) == 1 {
						ar.WindowAppeared = true
						expected, err = focusWindow(ctx, sig, b, input, matches[0], wait, checkpoint)
						ar.Foreground = err == nil
						if err == nil {
							ar.Status = "completed"
						}
						break
					}
					if time.Now().After(deadline) {
						err = errors.New("program started but expected window did not appear before timeout")
						break
					}
					err = wait(20 * time.Millisecond)
				}
			default:
				err = errors.New("unknown action type")
			}
		}
		if err == nil {
			err = sig.Check(ctx)
		}
		if err != nil {
			ar.Status = "partial"
			ar.Error = err.Error()
			res.Actions = append(res.Actions, ar)
			fail(i, err)
			break
		}
		res.Actions = append(res.Actions, ar)
		res.Completed++
	}
	if res.Error == "" {
		res.Status = "completed"
		err = wait(300 * time.Millisecond)
		if err != nil {
			fail(-1, err)
		}
	}
	if releaseErr := input.release(); releaseErr != nil {
		res.Status = "unknown"
		res.Error = releaseErr.Error()
	}
	return
}

func focusWindow(ctx context.Context, sig *Signal, b Backend, input *inputState, w protocol.DesktopWindow, wait func(time.Duration) error, check func() error) (protocol.DesktopWindow, error) {
	if w.Elevated || !w.ElevationKnown {
		return w, errors.New("cannot focus an elevated or unknown-integrity window")
	}
	if err := check(); err != nil {
		return w, err
	}
	if err := b.Focus(w); err != nil {
		return w, err
	}
	if current, err := b.Foreground(); err == nil && FocusGuard(w, current) == nil {
		return w, nil
	}
	// Foreground-lock handling belongs to the tool: release a synthetic Alt tap
	// before retrying SetForegroundWindow. No permanent lock or global override.
	if err := input.press(ctx, sig, held{key: 0x12}); err != nil {
		return w, err
	}
	if err := input.release(); err != nil {
		return w, err
	}
	if err := check(); err != nil {
		return w, err
	}
	if err := b.Focus(w); err != nil {
		return w, err
	}
	if err := wait(30 * time.Millisecond); err != nil {
		return w, err
	}
	current, err := b.Foreground()
	if err != nil {
		return w, err
	}
	if err = FocusGuard(w, current); err != nil {
		return w, fmt.Errorf("could not focus selected window: %w", err)
	}
	return w, nil
}

func containsTitle(title, fragment string) bool {
	return strings.Contains(strings.ToLower(title), strings.ToLower(fragment))
}
