package desktop

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"wanctl/internal/protocol"
)

// S24: the page observed physical mouse input after character 273, but the
// hook-only stop latch never saw it. Another 659 characters were sent, then
// the batch returned a display/session error at character 932. The physical
// device feed here is independent of the simulated (silent) hook, rather than
// directly setting Signal as the older stop tests did. Timing is compressed
// to one millisecond per character; the stop budget remains the real 200 ms.
type physicalInputBackend struct {
	*fakeBackend
	physical       chan struct{}
	at             time.Time
	monitorStarted bool
}

func (b *physicalInputBackend) WatchInput(sig *Signal) (func(), error) {
	b.monitorStarted = true
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-b.physical:
			sig.HumanInput(false)
		case <-stop:
		}
	}()
	return func() { close(stop); <-done }, nil
}
func (b *physicalInputBackend) Unicode(_ uint16, down bool) error {
	if err := b.fakeBackend.input(down); err != nil {
		return err
	}
	if down {
		if b.pressed == 273 {
			b.at = time.Now()
			close(b.physical)
		}
		time.Sleep(time.Millisecond)
	}
	return nil
}
func (b *physicalInputBackend) UnicodeTo(_ protocol.DesktopWindow, unit uint16, down bool) error {
	return b.Unicode(unit, down)
}
func (b *physicalInputBackend) Check(string) error {
	if b.pressed >= 932 {
		return errors.New("display configuration or session changed; take a fresh screenshot")
	}
	return nil
}
func TestPhysicalInputDuringLongTypeWinsOverDisplayError(t *testing.T) {
	for _, device := range []string{"mouse", "keyboard"} {
		t.Run(device, func(t *testing.T) {
			b := &physicalInputBackend{fakeBackend: fake(), physical: make(chan struct{})}
			// Both physical device classes use the same content-free signal; their
			// source is deliberately not the low-level hook which missed S24's input.
			res := (Engine{Backend: b}).Run(context.Background(), "owner", example(), []protocol.DesktopAction{{Type: "type", Text: strings.Repeat("x", 5000)}, {Type: "click", X: 64, Y: 80}})
			elapsed := time.Since(b.at)
			if b.at.IsZero() || res.Error != protocol.DesktopHumanInput || res.Status != "interrupted" || elapsed > 200*time.Millisecond {
				t.Fatalf("physical %s during type: error=%q status=%s post-input=%d latency=%v monitor_started=%v; want human stop within 200ms", device, res.Error, res.Status, b.pressed-273, elapsed, b.monitorStarted)
			}
			if res.Completed != 0 || res.FailedIndex != 0 || b.held != 0 || b.pressed != b.released || b.moves != 0 {
				t.Fatalf("queued input ran or held input was not released: %+v pressed=%d released=%d held=%d moves=%d", res, b.pressed, b.released, b.held, b.moves)
			}
			t.Logf("physical %s stopped type after %v; %d additional characters; held=%d", device, elapsed, b.pressed-273, b.held)
		})
	}
}

type latePhysicalInputBackend struct{ *fakeBackend }

func (b *latePhysicalInputBackend) WatchInput(sig *Signal) (func(), error) {
	// Model a device message already queued when the layout query returns; the
	// monitor's stop drains it before Engine chooses the public failure reason.
	return func() { sig.HumanInput(false) }, nil
}
func (b *latePhysicalInputBackend) Check(string) error {
	if b.pressed != 0 {
		return errors.New("display configuration or session changed")
	}
	return nil
}
func TestPhysicalInputReasonSurvivesQueuedDeliveryAndCleanup(t *testing.T) {
	b := &latePhysicalInputBackend{fake()}
	res := (Engine{Backend: b}).Run(context.Background(), "owner", example(), []protocol.DesktopAction{{Type: "type", Text: "abc"}, {Type: "click", X: 64, Y: 80}})
	if res.Error != protocol.DesktopHumanInput || res.Status != "interrupted" || len(res.Actions) != 1 || res.Actions[0].Error != protocol.DesktopHumanInput || b.held != 0 || b.moves != 0 {
		t.Fatalf("late physical input reported as a layout failure: %+v", res)
	}
}

// Exercise the same independent feed across wait and held-key/button phases,
// not just text. Fake native calls remain short, like the Backend contract.
type actionPhysicalBackend struct {
	*fakeBackend
	physical     chan struct{}
	stopAt       time.Time
	triggerAfter int
	checks       int
}

func (b *actionPhysicalBackend) WatchInput(sig *Signal) (func(), error) {
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-b.physical:
			sig.HumanInput(false)
		case <-stop:
		}
	}()
	return func() { close(stop); <-done }, nil
}
func (b *actionPhysicalBackend) Check(string) error {
	b.checks++
	if b.checks == b.triggerAfter {
		b.stopAt = time.Now()
		close(b.physical)
	}
	return nil
}
func TestPhysicalInputCancelsWaitAndReleasesHeldInputs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action protocol.DesktopAction
		at     int
	}{
		{"wait", protocol.DesktopAction{Type: "wait", Millis: 10000}, 4},
		{"held keys", protocol.DesktopAction{Type: "key", Key: "ctrl+shift+l"}, 6},
		{"held drag button", protocol.DesktopAction{Type: "drag", X: 100, Y: 100, ToX: 200, ToY: 200, Millis: 10000}, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &actionPhysicalBackend{fakeBackend: fake(), physical: make(chan struct{}), triggerAfter: tc.at}
			res := (Engine{Backend: b}).Run(context.Background(), "owner", example(), []protocol.DesktopAction{tc.action, {Type: "type", Text: "must not be sent"}})
			if b.stopAt.IsZero() || time.Since(b.stopAt) > 200*time.Millisecond || res.Error != protocol.DesktopHumanInput || res.Completed != 0 || len(res.Actions) != 1 || b.held != 0 || b.pressed != b.released {
				t.Fatalf("%s did not stop cleanly: %+v pressed=%d released=%d held=%d", tc.name, res, b.pressed, b.released, b.held)
			}
		})
	}
}
