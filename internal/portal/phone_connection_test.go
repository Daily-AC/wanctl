package portal

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"wanctl/internal/console"
	"wanctl/internal/protocol"
)

func connectedPhoneSpace(f *phoneFixture) {
	f.sup.spaces["alice"] = &phoneSpace{ns: "alice", phone: "phone-1", phoneName: "phone",
		online: true, session: f.phone, targets: map[string]*phoneTarget{}, names: map[string]string{"mac-1": "mac"}}
}

// A device connection becomes dead in its read goroutine, before the watch
// necessarily gets scheduled to consume its closed subscription. Meanwhile a
// phone reply can obtain a replacement session from deviceConnFor.
func TestLateTapBeforeWatchNoticesReconnect(t *testing.T) {
	f := newPhoneFixture(t)
	connectedPhoneSpace(f)
	var current atomic.Pointer[fakePhoneSession]
	current.Store(f.target)
	f.sup.sessionFor = func(context.Context, string, string) (phoneSession, error) { return current.Load(), nil }
	ctx, cancel := context.WithCancel(f.sup.ctx)
	observed, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	watch := residentWatch{
		ns: "alice", device: "mac-1", wait: phoneApprovalWait,
		sessionFor: func(context.Context, string, string) (deviceSession, error) { return current.Load(), nil },
		onDial:     func(error) error { return nil }, logf: t.Logf,
		retryMin: time.Millisecond, retryMax: time.Millisecond, waitRetry: waitContext,
		onDisconnect: func() {
			f.sup.mu.Lock()
			defer f.sup.mu.Unlock()
			f.sup.markDisappearanceUnknownLocked("alice", "mac-1")
		},
		onState: func(_ context.Context, session deviceSession, state console.State, wait time.Duration) {
			f.sup.onTargetState("alice", "mac-1", session, state, wait, map[string]string{}, map[string]string{})
			close(observed)
			<-resume // deterministic scheduling pause before observing disconnection
		},
	}
	go func() { defer close(done); _ = watch.run(ctx) }()
	t.Cleanup(func() { cancel(); close(resume); <-done })
	state := pendingState("p1", "make deploy")
	state.Pending[0].Cwd = "/"
	f.target.states <- state
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("watch did not publish the card")
	}
	card := f.phone.lastPush(t)
	// The request was answered elsewhere during the outage, before expiry.
	close(f.target.states)
	replacement := newFakePhoneSession()
	replacement.found = false
	current.Store(replacement)
	f.advance(phoneApprovalWait + time.Second)
	f.sup.handleReply("alice", "phone-1", protocol.Message{ApprovalID: card.ID, Verdict: "y"})
	_, _, grants, _ := replacement.snapshot()
	if got := f.phone.lastPush(t).Result; got != protocol.ResultHandled || len(grants) != 0 {
		t.Fatalf("tap during watch reconnect: result=%q grants=%v; want handled and no late grant", got, grants)
	}
}

func TestReconnectStillPendingCardCanBeDecided(t *testing.T) {
	f := newPhoneFixture(t)
	connectedPhoneSpace(f)
	seen := map[string]string{}
	state := pendingState("p1", "make deploy")
	state.Pending[0].Cwd = "/"
	f.sup.onTargetState("alice", "mac-1", f.target, state, phoneApprovalWait, seen, map[string]string{})
	card := f.phone.lastPush(t)
	f.sup.mu.Lock()
	f.sup.markDisappearanceUnknownLocked("alice", "mac-1")
	f.sup.mu.Unlock()
	// A short connection outage ends; the same request is still pending.
	f.advance(time.Second)
	f.sup.onTargetState("alice", "mac-1", f.target, state, phoneApprovalWait, seen, map[string]string{})
	f.sup.handleReply("alice", "phone-1", protocol.Message{ApprovalID: card.ID, Verdict: "y"})
	_, decisions, grants, _ := f.target.snapshot()
	if got := f.phone.lastPush(t).Result; got != protocol.ResultAllowed || len(decisions) != 1 || len(grants) != 0 {
		t.Fatalf("live pending request after reconnect: result=%q decisions=%v grants=%v; want ordinary approval", got, decisions, grants)
	}
}

// A real connection's close token is invalid before any watch callback runs.
// The fake RPC endpoint lets a delayed tap expose an incorrect late grant.
type observedPhoneSession struct {
	*fakePhoneSession
	generation <-chan struct{}
}

func (s *observedPhoneSession) approvalGeneration() <-chan struct{} { return s.generation }

func TestPendingPhoneCardRequiresOriginalConnectionForLateGrant(t *testing.T) {
	for _, mode := range []string{"closed", "missing on replacement"} {
		t.Run(mode, func(t *testing.T) {
			f := newPhoneFixture(t)
			connectedPhoneSpace(f)
			local, peer := net.Pipe()
			d := newDeviceConn(local)
			t.Cleanup(func() { d.close(); peer.Close() })
			original := &observedPhoneSession{f.target, d.closed}
			current := phoneSession(original)
			f.sup.sessionFor = func(context.Context, string, string) (phoneSession, error) { return current, nil }
			seen := map[string]string{}
			state := pendingState("p1", "make deploy")
			f.sup.onTargetState("alice", "mac-1", original, state, phoneApprovalWait, seen, map[string]string{})
			card := f.phone.lastPush(t)
			f.advance(phoneApprovalWait + time.Second)
			f.target.found = false
			if mode == "closed" {
				d.close()
			} else {
				replacement := newFakePhoneSession()
				replacement.found = false
				current = replacement
				f.sup.onTargetState("alice", "mac-1", current, console.State{}, phoneApprovalWait, seen, map[string]string{})
			}
			f.sup.handleReply("alice", "phone-1", protocol.Message{ApprovalID: card.ID, Verdict: "y"})
			if got := f.phone.lastPush(t).Result; got != protocol.ResultHandled {
				t.Fatalf("%s: result %q", mode, got)
			}
			_, _, grants, _ := f.target.snapshot()
			if len(grants) != 0 {
				t.Fatal(grants)
			}
		})
	}
}
