package desktop

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"unsafe"

	"wanctl/internal/protocol"
)

const lockedMessage = "desktop unavailable: screen locked or secure desktop active"

// Fake the WTS query and existing input-desktop check. Job execution and the
// action engine are real; capture/input counters represent OS side effects.
type lockStateBackend struct {
	*fakeBackend
	state                                                         int32
	desktopErr                                                    error
	queries, desktopChecks, captureCalls, captures, monitorStarts int
	lockAt                                                        string
	unlockAfterRead                                               bool
}

func (b *lockStateBackend) session() error {
	return checkSessionDesktop(1, func(session uint32) int32 {
		if session != 1 {
			panic("queried a different session")
		}
		b.queries++
		state := b.state
		if state == wtsSessionLocked && b.unlockAfterRead {
			b.state = wtsSessionUnlocked
		}
		return state
	}, func() error { b.desktopChecks++; return b.desktopErr })
}
func (b *lockStateBackend) WatchInput(sig *Signal) (func(), error) {
	b.monitorStarts++
	if b.lockAt == "physical-monitor" {
		b.state = wtsSessionLocked
	}
	if err := b.session(); err != nil {
		sig.failMonitor()
		return nil, err
	}
	return func() {}, nil
}
func (b *lockStateBackend) Begin(controller string, sig *Signal) (func(), error) {
	if b.lockAt == "indicator" {
		b.state = wtsSessionLocked
	}
	if err := b.session(); err != nil {
		return nil, err
	}
	return b.fakeBackend.Begin(controller, sig)
}
func (b *lockStateBackend) Check(string) error { return b.session() }
func (b *lockStateBackend) Capture(*protocol.Rect) (protocol.DesktopSnapshot, []byte, error) {
	b.captureCalls++
	if err := b.session(); err != nil {
		return protocol.DesktopSnapshot{}, nil, err
	}
	b.captures++
	return example(), []byte{0xff, 0xd8, 0xff, 0xd9}, nil
}
func lockTestJob(action string) Job {
	job := Job{Action: action}
	if action == "act" {
		snap := example()
		job.Reference = &snap
		job.Request.Actions = []protocol.DesktopAction{{Type: "wait", Millis: 10}, {Type: "type", Text: "x"}}
	}
	return job
}

func TestSessionLockRejectsScreenshotBeforeCapture(t *testing.T) {
	b := &lockStateBackend{fakeBackend: fake(), state: wtsSessionLocked}
	res, data := executeJobWithBackend(context.Background(), lockTestJob("screenshot"), b, b.session)
	if res.Status != "rejected" || res.Error != lockedMessage || res.Snapshot != nil || len(data) != 0 || b.captureCalls != 0 || b.desktopChecks != 0 {
		t.Fatalf("result=%+v bytes=%d captures=%d desktopChecks=%d", res, len(data), b.captures, b.desktopChecks)
	}
}

func TestSessionLockRejectsActBeforeInput(t *testing.T) {
	for _, action := range []protocol.DesktopAction{{Type: "wait", Millis: 10}, {Type: "type", Text: "must not be sent"}} {
		b := &lockStateBackend{fakeBackend: fake(), state: wtsSessionLocked}
		job := lockTestJob("act")
		job.Request.Actions = []protocol.DesktopAction{action}
		res, data := executeJobWithBackend(context.Background(), job, b, b.session)
		if res.Status != "rejected" || res.Error != lockedMessage || res.Completed != 0 || res.Snapshot != nil || len(data) != 0 || b.pressed != 0 || b.moves != 0 || b.begins != 0 || b.monitorStarts != 0 || b.captureCalls != 0 {
			t.Fatalf("%s: %+v bytes=%d input=%d begins=%d monitors=%d captures=%d", action.Type, res, len(data), b.pressed, b.begins, b.monitorStarts, b.captureCalls)
		}
	}
}

func TestSessionLockMidBatchStopsNextInputAndImage(t *testing.T) {
	for _, unlock := range []bool{false, true} {
		b := &lockStateBackend{fakeBackend: fake(), state: wtsSessionUnlocked, unlockAfterRead: unlock}
		b.onPress = func() { b.state = wtsSessionLocked }
		job := lockTestJob("act")
		job.Request.Actions = []protocol.DesktopAction{{Type: "type", Text: "ab"}, {Type: "click", X: 10, Y: 10}}
		res, data := executeJobWithBackend(context.Background(), job, b, b.session)
		if res.Status != "partial" || res.Error != lockedMessage || res.FailedIndex != 0 || res.Completed != 0 || res.Snapshot != nil || len(data) != 0 || b.pressed != 1 || b.released != 1 || b.held != 0 || b.moves != 0 || b.captureCalls != 0 {
			t.Fatalf("unlock=%v: %+v bytes=%d pressed=%d released=%d held=%d moves=%d captures=%d", unlock, res, len(data), b.pressed, b.released, b.held, b.moves, b.captureCalls)
		}
	}
}

func TestSessionLockDuringMonitorStartupKeepsExactError(t *testing.T) {
	for _, phase := range []string{"physical-monitor", "indicator"} {
		b := &lockStateBackend{fakeBackend: fake(), state: wtsSessionUnlocked, lockAt: phase}
		res, data := executeJobWithBackend(context.Background(), lockTestJob("act"), b, b.session)
		if res.Status != "rejected" || res.Error != lockedMessage || res.Snapshot != nil || len(data) != 0 || b.pressed != 0 || b.moves != 0 || b.captureCalls != 0 {
			t.Fatalf("%s: %+v bytes=%d pressed=%d moves=%d captures=%d", phase, res, len(data), b.pressed, b.moves, b.captureCalls)
		}
	}
}

func TestSessionStateFallbackRetainsDesktopChecks(t *testing.T) {
	for _, state := range []int32{wtsSessionUnlocked, wtsSessionUnknown} {
		for _, desktopErr := range []error{nil, ErrLocked} {
			for _, action := range []string{"screenshot", "act"} {
				b := &lockStateBackend{fakeBackend: fake(), state: state, desktopErr: desktopErr}
				res, data := executeJobWithBackend(context.Background(), lockTestJob(action), b, b.session)
				if b.queries == 0 || b.desktopChecks == 0 {
					t.Fatal("WTS or existing input-desktop check was bypassed")
				}
				if desktopErr != nil {
					if res.Error != lockedMessage || res.Status != "rejected" || res.Snapshot != nil || len(data) != 0 || b.pressed != 0 || b.captureCalls != 0 {
						t.Fatalf("secure desktop admitted: %+v", res)
					}
				} else if res.Status != "completed" || res.Error != "" || res.Snapshot == nil || len(data) == 0 || b.captures != 1 || (action == "act" && b.pressed != 1) {
					t.Fatalf("state=%d action=%s: %+v captures=%d pressed=%d", state, action, res, b.captures, b.pressed)
				}
			}
		}
	}
	// A failed query is represented as UNKNOWN, not a zero-initialized LOCK.
	if state := wtsSessionFlags(nil, 0, 1); state != wtsSessionUnknown {
		t.Fatalf("failed query state=%d", state)
	}
	if err := checkSessionDesktop(1, func(uint32) int32 { return wtsSessionFlags(nil, 0, 1) }, func() error { return ErrLocked }); !errors.Is(err, ErrLocked) {
		t.Fatalf("query failure bypassed secure-desktop check: %v", err)
	}
}

func TestWTSInfoExWAMD64Offsets(t *testing.T) {
	var info wtsInfoExPrefix
	if unsafe.Offsetof(info.Level) != 0 || unsafe.Offsetof(info.Data) != 8 || unsafe.Offsetof(info.Data)+unsafe.Offsetof(info.Data.SessionID) != 8 || unsafe.Offsetof(info.Data)+unsafe.Offsetof(info.Data.SessionState) != 12 || unsafe.Offsetof(info.Data)+unsafe.Offsetof(info.Data.SessionFlags) != 16 || unsafe.Sizeof(info) != 20 {
		t.Fatalf("WTSINFOEXW prefix: level=%d data=%d flags=%d size=%d", unsafe.Offsetof(info.Level), unsafe.Offsetof(info.Data), unsafe.Offsetof(info.Data)+unsafe.Offsetof(info.Data.SessionFlags), unsafe.Sizeof(info))
	}
	// Populate the documented ABI offsets, independently of our Go fields.
	for _, flags := range []int32{wtsSessionLocked, wtsSessionUnlocked, wtsSessionUnknown} {
		raw := make([]byte, 232)
		binary.LittleEndian.PutUint32(raw[0:4], 1)
		binary.LittleEndian.PutUint32(raw[8:12], 7)
		binary.LittleEndian.PutUint32(raw[12:16], 0) // WTSActive is not a lock flag
		binary.LittleEndian.PutUint32(raw[16:20], uint32(flags))
		p := (*wtsInfoExPrefix)(unsafe.Pointer(&raw[0]))
		if got := wtsSessionFlags(p, uint32(len(raw)), 7); got != flags {
			t.Fatalf("flags=%d got=%d", flags, got)
		}
	}
	info.Level, info.Data.SessionID = 1, 7
	for _, tc := range []struct{ size, session, level uint32 }{{19, 7, 1}, {20, 8, 1}, {20, 7, 2}} {
		info.Level = tc.level
		if got := wtsSessionFlags(&info, tc.size, tc.session); got != wtsSessionUnknown {
			t.Fatalf("invalid WTS result %+v got=%d", tc, got)
		}
	}
}
