package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"wanctl/internal/protocol"
	"wanctl/internal/sessionauth"
	"wanctl/internal/transport"
)

// pipeController is a paired controller talking to a over an in-memory
// connection, through the same handleSession a relayed session reaches: TLS,
// hello, trust, capabilities, dispatch. seed picks the controller identity, so
// two seeds are two different controllers.
func pipeController(t *testing.T, a *Agent, seed byte) *tls.Conn {
	t.Helper()
	id, err := transport.IdentityFromSeed(bytes.Repeat([]byte{seed}, 32), "pipe-controller")
	if err != nil {
		t.Fatal(err)
	}
	if !a.known.Has(id.Fingerprint) {
		if err := a.known.AddLabeled(id.Fingerprint, "pipe-controller", "test controller"); err != nil {
			t.Fatal(err)
		}
	}
	dev, controller := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); controller.Close(); dev.Close() })
	a.spawn(func() {
		a.handleSession(ctx, dev, sessionauth.Open{
			Session: "pipe-session", CallerNamespace: "owner", OwnerNamespace: "owner",
			Device: a.DeviceID(), Capabilities: sessionauth.UseCapabilities,
		})
	})
	hctx, hcancel := context.WithTimeout(ctx, 5*time.Second)
	defer hcancel()
	dr, err := transport.ClientHandshake(hctx, controller, "dev", id, transport.NewMemStore())
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	dr.Conn.SetDeadline(time.Now().Add(20 * time.Second))
	if err := protocol.WriteMessage(dr.Conn, protocol.Message{Kind: protocol.KindHello, Role: "client", Name: "pipe-controller", Label: "test controller", Version: "1"}); err != nil {
		t.Fatal(err)
	}
	if reply, err := protocol.ReadMessage(dr.Conn); err != nil || reply.Kind != protocol.KindOK {
		t.Fatalf("hello: %+v %v", reply, err)
	}
	return dr.Conn
}

// roundTrip sends one request and reads the one control message it answers
// with, skipping any data frames in between.
func roundTrip(t *testing.T, conn *tls.Conn, m protocol.Message) protocol.Message {
	t.Helper()
	if err := protocol.WriteMessage(conn, m); err != nil {
		t.Fatal(err)
	}
	for {
		kind, payload, err := protocol.ReadFrame(conn)
		if err != nil {
			t.Fatal(err)
		}
		if kind != protocol.FrameJSON {
			continue
		}
		reply, err := protocol.DecodeMessage(payload)
		if err != nil {
			t.Fatal(err)
		}
		return reply
	}
}
