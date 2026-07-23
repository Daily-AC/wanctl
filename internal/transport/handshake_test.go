package transport

import (
	"context"
	"net"
	"testing"
	"time"
)

func newID(t *testing.T) *Identity {
	t.Helper()
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	id, err := LoadOrCreateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestHandshakeOverPipe(t *testing.T) {
	serverID := newID(t)
	clientID := newID(t)
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	known, err := OpenStore("known_servers.json")
	if err != nil {
		t.Fatal(err)
	}

	a, b := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	type res struct {
		fp  string
		err error
	}
	srvCh := make(chan res, 1)
	go func() {
		conn, fp, err := ServerHandshake(ctx, a, serverID)
		if conn != nil {
			defer conn.Close()
		}
		srvCh <- res{fp, err}
	}()

	dr, err := ClientHandshake(ctx, b, "test-server", clientID, known)
	if err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	if !dr.FirstSeen {
		t.Fatal("expected FirstSeen on first contact")
	}
	if _, ok := known.GetByName("test-server"); ok {
		t.Fatal("first contact was persisted without explicit confirmation")
	}
	s := <-srvCh
	if s.err != nil {
		t.Fatalf("server handshake: %v", s.err)
	}
	if s.fp != clientID.Fingerprint {
		t.Fatalf("server saw client fp %q want %q", s.fp, clientID.Fingerprint)
	}
	if dr.PeerFP != serverID.Fingerprint {
		t.Fatalf("client saw server fp %q want %q", dr.PeerFP, serverID.Fingerprint)
	}
	dr.Conn.Close()
}
