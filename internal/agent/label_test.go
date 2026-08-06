package agent

import (
	"net"
	"testing"

	"wanctl/internal/eventlog"
	"wanctl/internal/protocol"
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

// A device's activity log recorded only connections that succeeded, so every
// refusal — unpaired, not a console admin, anonymous — left no trace on the
// device at all. The denials are the half you read when something cannot
// connect, and the half that would show someone probing.
func TestRefuseRecordsTheDenial(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	logger, err := eventlog.Open("events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{known: transport.NewMemStore(), log: logger}

	controller, device := net.Pipe()
	defer controller.Close()
	go a.refuse(device, "SHA256:stranger", "bogon", "rejected:unpaired",
		protocol.Message{Kind: protocol.KindReject, Reason: "device has not paired this controller"})
	if _, err := protocol.ReadMessage(controller); err != nil {
		t.Fatalf("controller never got the rejection: %v", err)
	}
	controller.Close()

	events, err := logger.Read(eventlog.Filter{Type: "connect"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("logged %d connect events, want 1", len(events))
	}
	e := events[0]
	if e.Decision != "rejected:unpaired" || e.PeerFP != "SHA256:stranger" || e.Detail == "" {
		t.Fatalf("event = %+v; want the fingerprint, the verdict and why", e)
	}
}
