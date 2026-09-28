package agent

import (
	"testing"

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
