package portal

import (
	"net"
	"testing"
	"time"

	"wanctl/internal/protocol"
)

// An approval phone resends every unsettled decision the moment a console
// session opens (agent approvalPhone.attach). The portal only subscribes after
// it has checked the session, so those resends arrive before anyone listens.
// They must still reach the first listener: a decision made while the phone
// was offline has no other way to the portal (S13, 2026-09-30 on PGBM10).
func TestDeviceConnKeepsReplyThatArrivesBeforeSubscribe(t *testing.T) {
	cli, srv := net.Pipe()
	defer srv.Close()
	d := newDeviceConn(cli)
	defer d.close()

	sent := make(chan error, 1)
	go func() {
		sent <- protocol.WriteMessage(srv, protocol.Message{Kind: protocol.KindApprovalReply, ApprovalID: "card-1", Verdict: "y"})
	}()
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // readLoop has handled it by now

	replies, cancel := d.replies()
	defer cancel()
	select {
	case m := <-replies:
		if m.ApprovalID != "card-1" || m.Verdict != "y" {
			t.Fatalf("got %+v", m)
		}
	case <-time.After(time.Second):
		t.Fatal("a reply sent before replies() was called never reached the listener")
	}
}
