package server

// A persistent session's shell is held inside an OS process container: its own
// process group on Unix, a job object on Windows. Cancelling a session command
// destroys the container, which ends the shell and everything it started, in
// one operation the kernel performs atomically. The session is then gone and
// the next command on that target builds a fresh one (issue #46, option (b)).
//
// The alternative — killing the command and keeping the shell — cannot be made
// correct when the shell is fed through stdin. The agent submits a whole line
// and the shell owns it; killing the child process it happens to be running
// leaves the rest of that line to execute in the surviving shell, so
// `sleep 600; rm -rf x` still runs the rm after a "cancel". Nor can the child
// be identified reliably: a pid/ppid snapshot proves nothing about descent once
// pids are reused. A container makes the unit of cancellation the thing the OS
// can actually name. See docs/adr/0011-session-cancel-resets-session.md.
//
// What escapes: on Unix a process that calls setsid() leaves the process group
// and survives. On Windows nothing escapes a job object unless it was created
// with breakaway rights, which this one is not.

import (
	"context"
	"errors"
	"sync"
)

// cancelGate binds a cancellation to exactly one request.
//
// The hard part is not firing the kill, it is guaranteeing that a kill armed by
// request A can never land on request B. Signalling a goroutine to stand down
// is not enough: it may already be past the check. So arming returns a disarm
// that takes the same lock the firing path holds, which means it *waits* for an
// in-flight cancel to finish and then reports whether it happened. The caller
// disarms while it still holds the session lock, so the next request cannot
// start until the previous one's cancellation has fully resolved either way.
type cancelGate struct {
	mu     sync.Mutex
	armed  bool
	fired  bool
	err    error
	kill   func() error
	stop   chan struct{}
	closed sync.Once
}

func newCancelGate(kill func() error) *cancelGate {
	return &cancelGate{kill: kill, stop: make(chan struct{})}
}

// arm watches ctx until the returned disarm is called. disarm reports whether
// the cancellation fired and what the kill returned.
func (g *cancelGate) arm(ctx context.Context) (disarm func() (bool, error)) {
	done := ctx.Done()
	if done == nil {
		return func() (bool, error) { return false, nil }
	}
	g.mu.Lock()
	g.armed, g.fired, g.err = true, false, nil
	g.mu.Unlock()

	g.stop = make(chan struct{})
	stop := g.stop
	go func() {
		select {
		case <-done:
			g.fire()
		case <-stop:
		}
	}()
	var once sync.Once
	return func() (bool, error) {
		once.Do(func() { close(stop) })
		g.mu.Lock()
		defer g.mu.Unlock()
		g.armed = false
		return g.fired, g.err
	}
}

// fire kills the container, unless the request that armed it has already been
// disarmed — in which case this cancellation belongs to a request that is over
// and must do nothing.
func (g *cancelGate) fire() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.armed {
		return
	}
	g.armed = false
	g.fired = true
	g.err = g.kill()
}

// ErrSessionCancelled is what a command reports when the controller stopped it.
// The session it ran in no longer exists: callers must discard it.
var ErrSessionCancelled = errors.New("session cancelled by the controller")
