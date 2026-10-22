package portal

import (
	"testing"
	"time"

	"wanctl/internal/console"
	"wanctl/internal/protocol"
)

// The confirmation mail is sent from the instance's own, DKIM-signed sender,
// so the link in it must point at the instance. handleEmailSend builds it from
// requestOrigin(r), which without PORTAL_PUBLIC_ORIGIN is the request's Host
// header; the same-origin check that guards the POST compares Origin with that
// same Host, so a client that sets both passes it. Mail is enabled whether or
// not a public origin is configured, and the door is open to any GitHub
// account, admitted or not: anyone can make the instance mail an arbitrary
// address a link to a host of their choosing, carrying a live token.

// A watch outage cannot turn an earlier decision elsewhere into a late grant.
func TestPhoneTapAfterAnswerElsewhereDuringOutageIsNotAGrant(t *testing.T) {
	f := newPhoneFixture(t)
	f.upAndWatching(t)

	f.target.states <- pendingState("p1", "git push --force origin main")
	phoneWaitFor(t, "push", func() bool { p, _, _, _ := f.phone.snapshot(); return len(p) == 1 })
	card := f.phone.lastPush(t)

	// The phone loses its network: the registry stops listing it and the
	// reconcile loop tears the watch down.
	f.mu.Lock()
	f.devices[0].Online = false
	f.mu.Unlock()
	if err := f.sup.reconcile(); err != nil {
		t.Fatal(err)
	}
	if _, online := f.sup.status("alice"); online {
		t.Fatal("phone still online")
	}

	// 20 s into the 180 s wait the owner approves the request on the portal
	// page; it runs and leaves the device. Nobody is watching for the phone.
	f.advance(20 * time.Second)
	f.target.mu.Lock()
	f.target.found = false
	f.target.mu.Unlock()

	// The phone is back after the original wait would have run out. The new
	// watch sees no pending request (it never knew p1), so nothing settles the
	// old card.
	f.advance(200 * time.Second)
	f.mu.Lock()
	f.devices[0].Online = true
	f.mu.Unlock()
	if err := f.sup.reconcile(); err != nil {
		t.Fatal(err)
	}
	if _, online := f.sup.status("alice"); !online {
		t.Fatal("phone not back")
	}
	f.target.states <- console.State{}
	time.Sleep(20 * time.Millisecond)

	// The tap the owner made on the still-pending notification (kept in the
	// phone's outbox, or made now on a card the phone never saw resolved).
	f.phone.replyCh <- protocol.Message{Kind: protocol.KindApprovalReply, ApprovalID: card.ID, Verdict: "y"}
	phoneWaitFor(t, "final card", func() bool {
		c := f.phone.lastPush(t)
		return c.ID == card.ID && c.State == protocol.ApprovalDone
	})
	if got := f.phone.lastPush(t).Result; got != protocol.ResultHandled {
		t.Fatalf("result = %q, want handled", got)
	}
	if _, _, g, _ := f.target.snapshot(); len(g) != 0 {
		t.Fatalf("a request answered elsewhere before its expiry became a late grant after a phone outage: %v (result %q)",
			g, f.phone.lastPush(t).Result)
	}
}
