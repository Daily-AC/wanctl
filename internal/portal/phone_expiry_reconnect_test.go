package portal

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"wanctl/internal/console"
	"wanctl/internal/protocol"
)

// The target uses real console framing and deviceConn lifecycle state. Closing
// an observation connection must not erase an expiry already established on it.
func TestObservedPhoneExpirySurvivesTargetReconnect(t *testing.T) {
	for _, reconnect := range []bool{false, true} {
		t.Run(map[bool]string{false: "same connection", true: "reconnected"}[reconnect], func(t *testing.T) {
			f := newPhoneFixture(t)
			connectedPhoneSpace(f)
			original, requests, notify := expiryTarget(t)
			current := original
			f.sup.sessionFor = func(context.Context, string, string) (phoneSession, error) { return current, nil }
			seen := map[string]string{}
			state := pendingState("p1", "make deploy")
			state.Pending[0].Cwd = "/srv/app"
			f.sup.onTargetState("alice", "mac-1", original, notify(state), phoneApprovalWait, seen, map[string]string{})
			card := f.phone.lastPush(t)
			f.advance(phoneApprovalWait + time.Second)
			f.sup.onTargetState("alice", "mac-1", original, notify(console.State{}), phoneApprovalWait, seen, map[string]string{})
			if got := f.phone.lastPush(t).State; got != protocol.ApprovalExpired {
				t.Fatalf("card state %q", got)
			}
			f.sup.mu.Lock()
			recorded := *f.sup.cards[card.ID]
			f.sup.mu.Unlock()
			if recorded.disappearedAt.IsZero() || recorded.disappearanceUnknown {
				t.Fatal("expiry was not conclusively observed")
			}
			if reconnect {
				original.close()
				current, requests, _ = expiryTarget(t)
				f.sup.mu.Lock()
				f.sup.markDisappearanceUnknownLocked("alice", "mac-1")
				f.sup.mu.Unlock()
			}
			f.sup.handleReply("alice", "phone-1", protocol.Message{ApprovalID: card.ID, Verdict: "y"})
			if got := f.phone.lastPush(t).Result; got != protocol.ResultGranted {
				t.Fatalf("known expiry: result=%q, RPCs=%d; want granted", got, len(requests))
			}
			if len(requests) != 1 {
				t.Fatalf("RPCs=%d, want one grant", len(requests))
			}
			msg := <-requests
			if msg.Kind != protocol.KindGrantOnce || msg.RuleKind != "exec" || msg.Pattern != card.Cmd || msg.FP != card.PeerFP || msg.Cwd != card.Cwd || msg.TimeoutSec != int(protocol.LateGrantWindow/time.Second) {
				t.Fatalf("grant binding changed: %+v", msg)
			}
			// A resend acknowledges the same completed card without a second grant.
			f.sup.handleReply("alice", "phone-1", protocol.Message{ApprovalID: card.ID, Verdict: "y"})
			if len(requests) != 0 {
				t.Fatal("duplicate grant")
			}
		})
	}
}

func expiryTarget(t *testing.T) (*deviceConn, chan protocol.Message, func(console.State) console.State) {
	t.Helper()
	local, remote := net.Pipe()
	d := newDeviceConn(local)
	requests := make(chan protocol.Message, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			msg, err := protocol.ReadMessage(remote)
			if err != nil {
				return
			}
			requests <- msg
			if err := protocol.WriteMessage(remote, protocol.Message{Kind: msg.Kind, Verdict: "not-found"}); err != nil {
				return
			}
		}
	}()
	states, unsubscribe := d.subscribe()
	t.Cleanup(func() { d.close(); remote.Close(); unsubscribe(); <-done })
	notify := func(state console.State) console.State {
		t.Helper()
		data, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		remote.SetWriteDeadline(time.Now().Add(3 * time.Second))
		if err := protocol.WriteMessage(remote, protocol.Message{Kind: protocol.KindApprovalNotif, Data: data}); err != nil {
			t.Fatal(err)
		}
		select {
		case st := <-states:
			return st
		case <-time.After(3 * time.Second):
			t.Fatal("notification was not delivered")
		}
		return console.State{}
	}
	return d, requests, notify
}
