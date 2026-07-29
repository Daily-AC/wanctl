package agent

import (
	"net"
	"testing"
	"time"

	"wanctl/internal/protocol"
)

// TestRejectHandshakeWaitsForController pins the ordering that a rejected
// handshake depends on. The relay pipes both directions and tears both down as
// soon as either ends, so a device that closes the moment it has written its
// rejection races those bytes through that teardown — the controller then
// reports EOF instead of the reason, which is what a shared CI runner caught as
// "error = EOF, want console capability rejection".
func TestRejectHandshakeWaitsForController(t *testing.T) {
	controller, device := net.Pipe()
	defer controller.Close()

	returned := make(chan struct{})
	go func() {
		rejectHandshake(device, protocol.Message{Kind: protocol.KindReject, Reason: "session capability denied: console"})
		close(returned)
	}()

	msg, err := protocol.ReadMessage(controller)
	if err != nil {
		t.Fatalf("read rejection: %v", err)
	}
	if msg.Kind != protocol.KindReject || msg.Reason != "session capability denied: console" {
		t.Fatalf("rejection = %+v", msg)
	}

	// Having written the rejection is not enough: the session must stay open
	// until the controller is done with it.
	select {
	case <-returned:
		t.Fatal("device released the session before the controller closed it")
	case <-time.After(100 * time.Millisecond):
	}

	controller.Close()
	select {
	case <-returned:
	case <-time.After(rejectHandshakeLinger + time.Second):
		t.Fatal("device held the session open after the controller closed it")
	}
}

// A controller that reads its rejection and then goes quiet must not pin the
// session open indefinitely.
func TestRejectHandshakeStopsWaitingEventually(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the linger deadline")
	}
	controller, device := net.Pipe()
	defer controller.Close()

	start := time.Now()
	returned := make(chan struct{})
	go func() {
		rejectHandshake(device, protocol.Message{Kind: protocol.KindReject, Reason: "denied"})
		close(returned)
	}()
	if _, err := protocol.ReadMessage(controller); err != nil {
		t.Fatalf("read rejection: %v", err)
	}

	select {
	case <-returned:
		if waited := time.Since(start); waited < rejectHandshakeLinger {
			t.Fatalf("gave up after %v, want at least %v", waited, rejectHandshakeLinger)
		}
	case <-time.After(rejectHandshakeLinger + 2*time.Second):
		t.Fatal("silent controller pinned the session open past the linger deadline")
	}
}
