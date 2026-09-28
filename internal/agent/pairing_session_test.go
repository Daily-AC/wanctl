package agent

import (
	"fmt"
	"strings"
	"testing"

	"wanctl/internal/eventlog"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
)

// Revoking a controller that was paired through the owner's approval has to
// stick. The approval is spent by the dial that collected it; if it were kept,
// the device would answer that same fingerprint "trusted" again on its next
// dial after the revocation, and nobody would be asked.
func TestRevokedControllerHasToPairAgain(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	a, err := New(Options{RelayURL: "http://unused", Token: "synthetic", Mode: policy.ModeNormal, Shell: "/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	id := seededIdentity(t, 7)

	if _, reply := pipeHello(t, a, id); reply.Kind != protocol.KindReject {
		t.Fatalf("an unknown controller was let in: %+v", reply)
	}
	if !a.console.DecidePair(id.Fingerprint, true) {
		t.Fatal("the pairing request was not waiting for the owner")
	}
	if _, reply := pipeHello(t, a, id); reply.Kind != protocol.KindOK {
		t.Fatalf("the approved controller was refused: %+v", reply)
	}
	if resp := a.handleConsoleRPC(protocol.Message{Kind: protocol.KindTrustRevoke, FP: id.Fingerprint}); len(resp.Data) != 0 {
		t.Fatalf("revoke: %s", resp.Data)
	}
	if _, reply := pipeHello(t, a, id); reply.Kind != protocol.KindReject {
		t.Fatalf("a revoked controller was trusted again without anyone being asked: %+v", reply)
	}
	if a.known.Has(id.Fingerprint) {
		t.Fatal("the revoked controller is back in the trust store")
	}
}

// With the owner's queue of pairing requests full, a new controller is turned
// away with a reason it can act on — later — and no pairing link, since nothing
// was queued for a link to approve. The device records the refusal.
func TestPairingRefusedWhileTheQueueIsFull(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	a, err := New(Options{RelayURL: "http://unused", Token: "synthetic", Mode: policy.ModeNormal, Shell: "/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	for i := 0; ; i++ {
		if _, err := a.console.AskPairNonBlocking(fmt.Sprintf("SHA256:waiting-%d", i), "w", "waiting"); err != nil {
			break
		}
		if i > 1000 {
			t.Fatal("the pairing queue never filled")
		}
	}
	id := seededIdentity(t, 9)
	_, reply := pipeHello(t, a, id)
	if reply.Kind != protocol.KindReject || !strings.Contains(reply.Reason, "too many pairing requests") || reply.PairingURL != "" {
		t.Fatalf("reply = %+v; want a reject that says to retry later, without a link", reply)
	}
	events, err := a.log.Read(eventlog.Filter{Type: "connect"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.PeerFP == id.Fingerprint && e.Decision == "rejected:pairing-full" {
			return
		}
	}
	t.Fatalf("no record of the refusal: %+v", events)
}
