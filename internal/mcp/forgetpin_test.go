package mcp

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"wanctl/internal/client"
	"wanctl/internal/transport"
)

// contact runs one real mutually-authenticated handshake against a device
// holding deviceID, and applies the pinning rules in known the way every dial
// does. It is the cheapest way to ask the question this test is about: after
// the owner unbinds a reinstalled device, is the next call first contact or a
// mismatch?
func contact(t *testing.T, name string, deviceID *transport.Identity, known *transport.Store) (*transport.DialResult, error) {
	t.Helper()
	a, b := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		conn, _, err := transport.ServerHandshake(ctx, a, deviceID)
		if err != nil {
			return
		}
		// net.Pipe is unbuffered, so the device has to keep reading or the
		// controller's close_notify blocks until the context expires.
		io.Copy(io.Discard, conn)
		conn.Close()
	}()
	controller := seededIdentity(t, "controller")
	dr, err := transport.ClientHandshake(ctx, b, name, controller, known)
	if dr != nil && dr.Conn != nil {
		dr.Conn.Close()
	}
	return dr, err
}

func seededIdentity(t *testing.T, seed string) *transport.Identity {
	t.Helper()
	raw := make([]byte, 32)
	copy(raw, seed)
	id, err := transport.IdentityFromSeed(raw, "wanctl-test:"+seed)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// A reinstalled device has a new key pair, so every call fails the pin. Nothing
// in the hosted store can be edited by hand — it is process memory — so before
// this hook the only way out was restarting the relay. Unbinding the device is
// the owner saying over SSO that the name no longer refers to that machine, and
// that is what clears it.
func TestUnbindingLetsAReinstalledDeviceBeConfirmedAgain(t *testing.T) {
	sessions = &sessionStore{
		seed:  []byte(testSeed),
		m:     map[string]*remoteSession{},
		trust: map[string]*transport.Store{},
	}
	sessions.mu.Lock()
	known := sessions.trustForLocked("alice")
	sessions.mu.Unlock()

	before := seededIdentity(t, "device-before-reinstall")
	if err := known.Pin("alice/build", before.Fingerprint, false); err != nil {
		t.Fatal(err)
	}

	after := seededIdentity(t, "device-after-reinstall")
	_, err := contact(t, "alice/build", after, known)
	var mismatch *transport.MismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("reinstalled device should fail the pin, got %v", err)
	}
	if mismatch.Stored != before.Fingerprint || mismatch.Offered != after.Fingerprint {
		t.Fatalf("mismatch = %+v", mismatch)
	}

	// The owner unbinds it in the portal; the relay calls this.
	ForgetPinnedDevice("alice", "build")

	dr, err := contact(t, "alice/build", after, known)
	if err != nil {
		t.Fatalf("after unbinding, the next contact still fails: %v", err)
	}
	if !dr.FirstSeen {
		t.Fatal("after unbinding, the device is still pinned to its old identity")
	}
	if _, stillPinned := known.GetByName("alice/build"); stillPinned {
		t.Fatal("the pin survived the unbind")
	}

	// And first contact is what the model is told, so it can clear it itself.
	text := toolText(dialErrorResult(&remoteSession{},
		&client.TrustRequiredError{Target: "alice/build", Fingerprint: dr.PeerFP}))
	if !strings.Contains(text, "DEVICE IDENTITY CONFIRMATION REQUIRED") ||
		!strings.Contains(text, "wanctl_trust_server") ||
		!strings.Contains(text, after.Fingerprint) {
		t.Fatalf("recovery result:\n%s", text)
	}
}

// One machine is routinely pinned under several names, so forgetting one name
// must leave the others alone.
func TestForgettingOneNameLeavesOtherPinsAlone(t *testing.T) {
	sessions = &sessionStore{m: map[string]*remoteSession{}, trust: map[string]*transport.Store{}}
	sessions.mu.Lock()
	alice := sessions.trustForLocked("alice")
	bob := sessions.trustForLocked("bob")
	sessions.mu.Unlock()

	fp := seededIdentity(t, "one-machine").Fingerprint
	for _, name := range []string{"alice/build", "alice/legion"} {
		if err := alice.Pin(name, fp, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := bob.Pin("bob/build", fp, false); err != nil {
		t.Fatal(err)
	}

	ForgetPinnedDevice("alice", "build")

	if _, ok := alice.GetByName("alice/build"); ok {
		t.Error("alice/build was not forgotten")
	}
	if _, ok := alice.GetByName("alice/legion"); !ok {
		t.Error("forgetting one name dropped the machine's other name")
	}
	if _, ok := bob.GetByName("bob/build"); !ok {
		t.Error("another namespace's pin for the same device name was dropped")
	}
}

// A session-keyed login (no OAuth bearer) keeps its own store, so the hook has
// to reach that one too.
func TestForgettingReachesSessionKeyedLogins(t *testing.T) {
	own := transport.NewMemStore()
	sessions = &sessionStore{
		m:     map[string]*remoteSession{"sid-1": {id: "sid-1", namespace: "alice", known: own}},
		trust: map[string]*transport.Store{},
	}
	fp := seededIdentity(t, "session-keyed").Fingerprint
	if err := own.Pin("alice/build", fp, false); err != nil {
		t.Fatal(err)
	}
	ForgetPinnedDevice("alice", "build")
	if _, ok := own.GetByName("alice/build"); ok {
		t.Error("a session-keyed login kept the stale pin")
	}
}

// Calling it before any handler exists (stdio, or a relay with MCP off) must
// not panic.
func TestForgettingIsSafeWithoutAHostedServer(t *testing.T) {
	sessions = nil
	ForgetPinnedDevice("alice", "build")
	sessions = &sessionStore{stdio: &localFsSession{}}
	ForgetPinnedDevice("alice", "build")
}
