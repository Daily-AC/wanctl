package desktop

import (
	"context"
	"errors"
	"strings"

	"wanctl/internal/protocol"
)

var errNoConsoleUser = errors.New("desktop unavailable: no signed-in user at the active console (remote-desktop sessions are not selected)")

// These calls are the session-selection boundary; tests do not need Windows
// or a real user's desktop. The native token owns its launcher and cleanup.
type sessionCalls struct {
	processSession func() (uint32, error)
	consoleSession func() uint32
	consoleUser    func(uint32) (consoleUser, error)
}

type consoleUser struct {
	logon string
	start helperStart
	close func()
}

func runSessionHelper(ctx context.Context, job Job, calls sessionCalls, inherited helperStart) (protocol.DesktopResult, []byte, error) {
	reject := func(err error) (protocol.DesktopResult, []byte, error) {
		return protocol.DesktopResult{Status: "rejected", Error: err.Error(), FailedIndex: -1}, nil, nil
	}
	session, err := calls.processSession()
	if err != nil {
		return reject(err)
	}
	if session != 0 {
		return exchangeHelper(ctx, job, func(ctx context.Context) (*helperProcess, error) {
			p, err := inherited(ctx)
			if err == nil {
				RecordSession(ctx, session)
			}
			return p, err
		})
	}
	session = calls.consoleSession()
	if session == 0 || session == ^uint32(0) {
		return reject(errNoConsoleUser)
	}
	user, err := calls.consoleUser(session)
	if err != nil {
		return reject(err)
	}
	defer user.close()
	// The logon SID changes even when Windows reuses a session number after
	// sign-out. Keep it in the existing opaque layout field, outside the helper;
	// the helper's display/focus guards and wire framing stay unchanged.
	suffix := ":" + user.logon
	if job.Reference != nil {
		if job.Reference.Session != session || !strings.HasSuffix(job.Reference.Layout, suffix) {
			return reject(errors.New("stale screenshot_id: console session or signed-in user changed; take a fresh screenshot"))
		}
		ref := *job.Reference
		ref.Layout = strings.TrimSuffix(ref.Layout, suffix)
		job.Reference = &ref
	}
	if calls.consoleSession() != session {
		return reject(errors.New("desktop unavailable: active console session changed; take a fresh screenshot"))
	}
	res, data, err := exchangeHelper(ctx, job, user.start)
	if res.Snapshot != nil {
		res.Snapshot.Layout += suffix
	}
	return res, data, err
}

type sessionObserverKey struct{}

// WithSessionObserver records local execution metadata without changing the
// desktop protocol. The observer runs synchronously before the runner returns.
func WithSessionObserver(ctx context.Context, record func(uint32)) context.Context {
	return context.WithValue(ctx, sessionObserverKey{}, record)
}

// RecordSession is called only after a helper process has actually started.
func RecordSession(ctx context.Context, session uint32) {
	if record, ok := ctx.Value(sessionObserverKey{}).(func(uint32)); ok {
		record(session)
	}
}
