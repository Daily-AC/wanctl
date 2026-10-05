package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"wanctl/internal/protocol"
)

// Fake only the Windows selection/creation calls. The real request/result
// framing, service store, coordinate consumption and runner all execute.
func fakeHelper(t *testing.T, execute func(Job) protocol.DesktopResult) helperStart {
	t.Helper()
	return func(context.Context) (*helperProcess, error) {
		in, send := io.Pipe()
		read, out := io.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer in.Close()
			defer out.Close()
			kind, data, err := protocol.ReadFrame(in)
			if err != nil {
				return
			}
			var job Job
			if kind != protocol.FrameJSON || json.Unmarshal(data, &job) != nil {
				t.Error("invalid helper request")
				return
			}
			if err := WriteResult(out, execute(job), nil); err != nil {
				t.Errorf("helper result: %v", err)
			}
		}()
		return &helperProcess{stdin: send, stdout: read, wait: func() error { <-done; return nil }, kill: func() error { in.Close(); return out.Close() }}, nil
	}
}

func noInheritedHelper(t *testing.T) helperStart {
	return func(context.Context) (*helperProcess, error) {
		t.Error("session-0 helper inherited the service's desktop/token")
		return nil, errors.New("wrong helper launch")
	}
}

func TestSessionZeroSelectsActiveConsoleUser(t *testing.T) {
	var queried, recorded uint32
	closed, started := false, false
	calls := sessionCalls{
		processSession: func() (uint32, error) { return 0, nil },
		consoleSession: func() uint32 { return 7 }, // no enumeration/fallback to RDP
		consoleUser: func(id uint32) (consoleUser, error) {
			queried = id
			start := fakeHelper(t, func(job Job) protocol.DesktopResult {
				if job.Action != "screenshot" {
					t.Errorf("job: %+v", job)
				}
				return protocol.DesktopResult{Status: "completed", Snapshot: &protocol.DesktopSnapshot{Session: id, Layout: "physical-layout"}}
			})
			return consoleUser{logon: "S-1-5-5-10-20", close: func() { closed = true }, start: func(ctx context.Context) (*helperProcess, error) {
				p, err := start(ctx)
				started = true
				RecordSession(ctx, id)
				return p, err
			}}, nil
		},
	}
	ctx := WithSessionObserver(context.Background(), func(id uint32) { recorded = id })
	res, _, err := runSessionHelper(ctx, Job{Action: "screenshot"}, calls, noInheritedHelper(t))
	if err != nil || res.Status != "completed" || !started || !closed || queried != 7 || recorded != 7 {
		t.Fatalf("res=%+v err=%v started=%v closed=%v queried=%d recorded=%d", res, err, started, closed, queried, recorded)
	}
	if res.Snapshot.Layout != "physical-layout:S-1-5-5-10-20" {
		t.Fatalf("missing logon binding: %+v", res.Snapshot)
	}
}

func TestSessionZeroNoSignedInConsoleUser(t *testing.T) {
	for _, id := range []uint32{0, ^uint32(0), 3} {
		t.Run(strconv.FormatUint(uint64(id), 10), func(t *testing.T) {
			calls := sessionCalls{
				processSession: func() (uint32, error) { return 0, nil },
				consoleSession: func() uint32 { return id },
				consoleUser: func(got uint32) (consoleUser, error) {
					if id != 3 || got != id {
						t.Errorf("queried non-console user %d", got)
					}
					return consoleUser{}, errNoConsoleUser // WTSQueryUserToken ERROR_NO_TOKEN; RDP may exist
				},
			}
			res, _, err := runSessionHelper(context.Background(), Job{Action: "screenshot"}, calls, noInheritedHelper(t))
			if err != nil || res.Status != "rejected" || !strings.Contains(res.Error, "no signed-in user at the active console") {
				t.Fatalf("%+v %v", res, err)
			}
		})
	}
}

func TestSessionZeroLockedScreenPreservesError(t *testing.T) {
	const locked = "desktop unavailable: screen locked or secure desktop active"
	calls := sessionCalls{
		processSession: func() (uint32, error) { return 0, nil },
		consoleSession: func() uint32 { return 5 },
		consoleUser: func(uint32) (consoleUser, error) {
			return consoleUser{logon: "login", close: func() {}, start: fakeHelper(t, func(Job) protocol.DesktopResult {
				return protocol.DesktopResult{Status: "rejected", Error: locked, FailedIndex: -1}
			})}, nil
		},
	}
	var service Service
	res, _ := service.Do(context.Background(), "owner", "controller", "screenshot", NewID(), &protocol.DesktopRequest{}, func(ctx context.Context, job Job) (protocol.DesktopResult, []byte, error) {
		return runSessionHelper(ctx, job, calls, noInheritedHelper(t))
	})
	if res.Status != "rejected" || res.Error != locked || res.Snapshot != nil {
		t.Fatalf("%+v", res)
	}
}

func TestSessionZeroSessionChangeRejectsOldCoordinates(t *testing.T) {
	for _, change := range []string{"fast-user-switch", "sign-out-session-number-reused", "same-logon"} {
		t.Run(change, func(t *testing.T) {
			session, logon := uint32(4), "logon-A"
			starts := 0
			calls := sessionCalls{
				processSession: func() (uint32, error) { return 0, nil },
				consoleSession: func() uint32 { return session },
				consoleUser: func(uint32) (consoleUser, error) {
					return consoleUser{logon: logon, close: func() {}, start: fakeHelper(t, func(job Job) protocol.DesktopResult {
						starts++
						if job.Reference != nil && job.Reference.Layout != "layout" {
							t.Errorf("helper saw wrapped layout: %+v", job.Reference)
						}
						return protocol.DesktopResult{Status: "completed", Snapshot: &protocol.DesktopSnapshot{Session: session, Layout: "layout"}}
					})}, nil
				},
			}
			var service Service
			run := func(ctx context.Context, job Job) (protocol.DesktopResult, []byte, error) {
				return runSessionHelper(ctx, job, calls, noInheritedHelper(t))
			}
			shot, _ := service.Do(context.Background(), "owner", "controller", "screenshot", NewID(), &protocol.DesktopRequest{}, run)
			if shot.Snapshot == nil {
				t.Fatalf("capture: %+v", shot)
			}
			if change == "fast-user-switch" {
				session, logon = 8, "logon-B"
			}
			if change == "sign-out-session-number-reused" {
				logon = "logon-C"
			}
			res, _ := service.Do(context.Background(), "owner", "controller", "act", NewID(), &protocol.DesktopRequest{ScreenshotID: shot.Snapshot.ID, Actions: []protocol.DesktopAction{{Type: "click", X: 20, Y: 30}}}, run)
			if change == "same-logon" {
				if res.Status != "completed" || starts != 2 {
					t.Fatalf("unchanged desktop rejected: %+v starts=%d", res, starts)
				}
			} else if res.Status != "rejected" || !strings.Contains(res.Error, "stale screenshot_id") || starts != 1 {
				t.Fatalf("old coordinates reached another session: %+v starts=%d", res, starts)
			}
		})
	}
}

func TestDesktopSessionKeepsInheritedHelper(t *testing.T) {
	calls := sessionCalls{
		processSession: func() (uint32, error) { return 12, nil },
		consoleSession: func() uint32 { t.Error("desktop agent selected another session"); return 99 },
		consoleUser: func(uint32) (consoleUser, error) {
			t.Error("desktop agent queried another user's token")
			return consoleUser{}, errNoConsoleUser
		},
	}
	inherited := fakeHelper(t, func(job Job) protocol.DesktopResult {
		if job.Reference.Layout != "old-layout" || job.Request.Actions[0].Text != "private" {
			t.Errorf("job changed: %+v", job)
		}
		return protocol.DesktopResult{Status: "completed", Snapshot: &protocol.DesktopSnapshot{Session: 12, Layout: "old-layout"}}
	})
	job := Job{Action: "act", Reference: &protocol.DesktopSnapshot{Session: 12, Layout: "old-layout"}, Request: protocol.DesktopRequest{Actions: []protocol.DesktopAction{{Type: "type", Text: "private"}}}}
	res, _, err := runSessionHelper(context.Background(), job, calls, inherited)
	if err != nil || res.Status != "completed" || res.Snapshot.Layout != "old-layout" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestSessionZeroConsoleChangesBeforeLaunch(t *testing.T) {
	console, closed := uint32(2), false
	calls := sessionCalls{
		processSession: func() (uint32, error) { return 0, nil },
		consoleSession: func() uint32 { return console },
		consoleUser: func(uint32) (consoleUser, error) {
			console = 3
			return consoleUser{logon: "login", close: func() { closed = true }, start: noInheritedHelper(t)}, nil
		},
	}
	res, _, err := runSessionHelper(context.Background(), Job{Action: "screenshot"}, calls, noInheritedHelper(t))
	if err != nil || !closed || !strings.Contains(res.Error, "active console session changed") {
		t.Fatalf("%+v %v closed=%v", res, err, closed)
	}
}

func TestSessionZeroCancellationClosesInput(t *testing.T) {
	in, send := io.Pipe()
	read, out := io.Pipe()
	entered, eof, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		defer out.Close()
		defer in.Close()
		_, _, _ = protocol.ReadFrame(in)
		close(entered)
		var one [1]byte
		if _, err := in.Read(one[:]); err == io.EOF {
			close(eof)
		}
		_ = WriteResult(out, protocol.DesktopResult{Status: "interrupted"}, nil)
	}()
	calls := sessionCalls{
		processSession: func() (uint32, error) { return 0, nil },
		consoleSession: func() uint32 { return 2 },
		consoleUser: func(uint32) (consoleUser, error) {
			return consoleUser{logon: "login", close: func() {}, start: func(context.Context) (*helperProcess, error) {
				return &helperProcess{stdin: send, stdout: read, wait: func() error { <-done; return nil }, kill: func() error { return errors.New("unexpected forced kill") }}, nil
			}}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-entered; cancel() }()
	res, _, err := runSessionHelper(ctx, Job{Action: "screenshot"}, calls, noInheritedHelper(t))
	if err != nil || res.Status != "interrupted" {
		t.Fatalf("%+v %v", res, err)
	}
	select {
	case <-eof:
	case <-time.After(time.Second):
		t.Fatal("helper did not receive EOF")
	}
}
