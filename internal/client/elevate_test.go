package client

import (
	"strings"
	"testing"

	"wanctl/internal/protocol"
)

// An agent older than elevation support ignores the `elevate` field, runs the
// command unprivileged, and answers with an ordinary exit — a success that is
// indistinguishable from the real thing except for the missing channel name.
func TestUnelevatedExitFromAnOldAgentIsAnError(t *testing.T) {
	err := elevationHonoured(
		ExecRequest{Target: "phone", Command: "pm list packages", Elevate: true},
		protocol.Message{Kind: protocol.KindExit, Code: 0},
	)
	if err == nil {
		t.Fatal("an exit with no channel was accepted as an elevated run")
	}
	// The command already ran; a message implying otherwise would mislead.
	if !strings.Contains(err.Error(), "it ran with the agent's own privileges") {
		t.Fatalf("error = %q, want it to say the command already ran", err)
	}
	if !strings.Contains(err.Error(), "phone") {
		t.Fatalf("error = %q, want it to name the device", err)
	}
}

func TestElevatedExitWithAChannelIsAccepted(t *testing.T) {
	if err := elevationHonoured(
		ExecRequest{Target: "phone", Elevate: true},
		protocol.Message{Kind: protocol.KindExit, Code: 0, ElevatedVia: "su"},
	); err != nil {
		t.Fatalf("a properly elevated exit was rejected: %v", err)
	}
}

func TestOrdinaryExecIsUnaffected(t *testing.T) {
	// No elevation asked for, no channel echoed: the ordinary case must not
	// start failing.
	if err := elevationHonoured(
		ExecRequest{Target: "phone", Command: "echo hi"},
		protocol.Message{Kind: protocol.KindExit, Code: 0},
	); err != nil {
		t.Fatalf("ordinary exec rejected: %v", err)
	}
}

func TestNonZeroExitStillReportsTheElevationFailure(t *testing.T) {
	// A command that failed AND was not elevated must report the elevation
	// problem: "exit 1" alone would be read as the command's own verdict.
	err := elevationHonoured(
		ExecRequest{Target: "phone", Elevate: true},
		protocol.Message{Kind: protocol.KindExit, Code: 1},
	)
	if err == nil {
		t.Fatal("a failed, unelevated command was reported as a plain failure")
	}
	if !strings.Contains(err.Error(), "exit 1") {
		t.Fatalf("error = %q, want it to carry the exit code", err)
	}
}
