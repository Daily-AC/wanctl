package agent

import (
	"testing"

	"wanctl/internal/transport"
)

func agentWithKnown(t *testing.T, autoYes bool) (*Agent, *transport.Store) {
	t.Helper()
	known := transport.NewMemStore()
	return &Agent{known: known, opts: Options{AutoYes: autoYes}}, known
}

// An unknown controller that will not say who it is would reach the owner as
// "trust SHA256:… from bogon?" — a prompt with nothing in it to decide on. Refuse
// it at the device instead of turning it into a click-through.
func TestUnlabeledControllerIsRefusedBeforePairing(t *testing.T) {
	a, _ := agentWithKnown(t, false)
	for _, label := range []string{"", "   ", "\t\n"} {
		if !a.unlabeledPairing("SHA256:stranger", label) {
			t.Fatalf("label %q was accepted; it carries no information for the owner", label)
		}
	}
	if a.unlabeledPairing("SHA256:stranger", "张三的 MacBook / Claude Code") {
		t.Fatal("a labeled controller must be allowed to raise a pairing request")
	}
}

// The gate exists to make a pairing decision answerable, so it must not touch
// controllers that are past that decision — otherwise upgrading the device
// would cut off every controller paired before labels were required.
func TestAlreadyTrustedControllerNeedsNoLabel(t *testing.T) {
	a, known := agentWithKnown(t, false)
	fp := transport.Fingerprint([]byte("long-standing controller"))
	if err := known.Add(fp, "laptop"); err != nil {
		t.Fatal(err)
	}
	if a.unlabeledPairing(fp, "") {
		t.Fatal("an already-trusted controller was refused for having no label")
	}
}

// --yes is an operator saying "trust whatever connects" at the console. There is
// no human to inform, so there is nothing for a label to make answerable.
func TestAutoYesSkipsTheLabelRequirement(t *testing.T) {
	a, _ := agentWithKnown(t, true)
	if a.unlabeledPairing("SHA256:stranger", "") {
		t.Fatal("--yes must keep bootstrapping unattended devices")
	}
}
