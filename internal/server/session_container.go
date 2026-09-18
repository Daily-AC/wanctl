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
// request A can never land on request B. Two things are needed and neither is
// sufficient alone.
//
// Signalling a watcher to stand down does not stop it: it may already be past
// the check. So disarming takes the same lock the firing path holds, which
// means it waits for an in-flight cancellation to finish and then reports
// whether it happened. The caller disarms while it still holds the session
// lock, so the next request cannot start until the previous one's
// cancellation has fully resolved either way.
//
// Waiting is still not enough, because a watcher can wake *after* its own
// request disarmed and after the next one armed, and a shared "is anything
// armed" flag reads true in that state — measured at 50 misfires in 100 runs.
// So every arming takes a generation number, the watcher carries the one it
// was armed with, and firing does nothing unless that generation is still the
// live one. A late watcher names a request that is over and is ignored.
type cancelGate struct {
	mu       sync.Mutex
	next     uint64 // generation counter; every arm takes the next value
	live     uint64 // the generation currently armed, 0 when none is
	firedGen uint64 // the generation a cancellation actually fired for
	err      error
	kill     func() error
}

func newCancelGate(kill func() error) *cancelGate {
	return &cancelGate{kill: kill}
}

// arm watches ctx on behalf of one request until the returned disarm is called.
// disarm reports whether the cancellation fired for *this* request and what the
// kill returned.
func (g *cancelGate) arm(ctx context.Context) (disarm func() (bool, error)) {
	done := ctx.Done()
	if done == nil {
		return func() (bool, error) { return false, nil }
	}
	g.mu.Lock()
	g.next++
	mine := g.next
	g.live = mine
	g.mu.Unlock()

	stop := make(chan struct{})
	go func() {
		select {
		case <-done:
			g.fire(mine)
		case <-stop:
		}
	}()
	var once sync.Once
	return func() (bool, error) {
		once.Do(func() { close(stop) })
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.live == mine {
			g.live = 0
		}
		if g.firedGen != mine {
			return false, nil
		}
		return true, g.err
	}
}

// fire kills the container on behalf of generation gen, unless that generation
// is no longer the armed one — in which case this cancellation belongs to a
// request that is over and must do nothing.
func (g *cancelGate) fire(gen uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if gen == 0 || g.live != gen {
		return
	}
	g.live = 0
	g.firedGen = gen
	g.err = g.kill()
}

// ErrSessionCancelled is what a command reports when the controller stopped it.
// The session it ran in no longer exists: callers must discard it.
var ErrSessionCancelled = errors.New("session cancelled by the controller")

// ErrSessionUnusable says the session was already gone when the request reached
// it, so nothing was submitted to the device. It is the one failure a caller may
// safely answer by acquiring a fresh session and running the command once:
// every other error leaves open the possibility that the command did run.
var ErrSessionUnusable = errors.New("session closed before the command was submitted")
