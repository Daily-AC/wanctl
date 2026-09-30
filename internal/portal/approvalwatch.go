package portal

import (
	"context"
	"time"

	"wanctl/internal/console"
)

// residentWatch keeps one console subscription to a device alive for as long as
// its context lives: dial, raise the device's approval wait, hand every state
// the device pushes to onState, and re-dial with bounded backoff when the
// session drops. When it returns it restores the device's default wait.
//
// It is the part of the Feishu workflow that has nothing to do with Feishu.
// While it runs the device has a front-end, so a request no rule covers waits
// for a human instead of being refused on the spot; what the watch does with a
// pending request — a Feishu card, a push to the owner's phone — is the
// outlet's business (ADR 0015).
type residentWatch struct {
	ns, device string
	wait       time.Duration

	sessionFor sessionForFunc
	retryMin   time.Duration
	retryMax   time.Duration
	waitRetry  func(context.Context, time.Duration) bool
	logf       func(string, ...any)
	logPrefix  string // "lark approval", "approval phone": names the outlet in log lines

	// onDial sees every dial outcome. A non-nil return ends the watch with that
	// error; nil keeps retrying (after a failure) or proceeds (after a success).
	onDial func(err error) error
	// onState runs for every state the device pushes, with the wait the device
	// actually applied.
	onState func(ctx context.Context, session deviceSession, state console.State, wait time.Duration)
}

func (w residentWatch) run(ctx context.Context) error {
	backoff := w.retryMin
	var lastSession deviceSession
	defer func() {
		if lastSession != nil {
			if _, err := lastSession.setApprovalTimeout(0); err != nil {
				w.logf("%s restore timeout for %s: %v", w.logPrefix, larkDeviceKey(w.ns, w.device), err)
			}
		}
	}()

	for {
		session, err := w.sessionFor(ctx, w.ns, w.device)
		if err != nil {
			if stop := w.onDial(err); stop != nil {
				return stop
			}
			if !w.waitRetry(ctx, backoff) {
				return nil
			}
			backoff = nextBackoff(backoff, w.retryMax)
			continue
		}
		if stop := w.onDial(nil); stop != nil {
			return stop
		}
		lastSession = session
		// A device older than the timeout_set verb answers "unknown RPC kind".
		// That must not stop us watching it: carrying on with whatever wait the
		// device already uses degrades the feature to a shorter window, whereas
		// treating it as a dial failure would make approvals silently never
		// work on every agent that has not been upgraded yet.
		wait := w.wait
		if applied, err := session.setApprovalTimeout(int(w.wait / time.Second)); err != nil {
			wait = console.DefaultTimeout
			w.logf("%s timeout for %s not set, using the device default %s: %v",
				w.logPrefix, larkDeviceKey(w.ns, w.device), wait, err)
		} else if applied > 0 {
			// The device clamps, so the outlet must state what it actually
			// applied rather than what we asked for.
			wait = time.Duration(applied) * time.Second
		}

		states, unsubscribe := session.subscribe()
		backoff = w.retryMin
		closed := false
		for !closed {
			select {
			case <-ctx.Done():
				unsubscribe()
				return nil
			case state, ok := <-states:
				if !ok {
					closed = true
					continue
				}
				w.onState(ctx, session, state, wait)
			}
		}
		unsubscribe()
		// A closed channel means visibility was lost, not that pending work
		// disappeared. The outlet keeps what it has seen across the re-dial.
		if !w.waitRetry(ctx, backoff) {
			return nil
		}
		backoff = nextBackoff(backoff, w.retryMax)
	}
}
