package agent

import (
	"strings"
	"testing"
	"time"

	"wanctl/internal/console"
	"wanctl/internal/eventlog"
	"wanctl/internal/policy"
)

// What a controller says about itself arrives with the hello and goes on to
// the owner's pairing card, the trust list, the activity log and
// notifications. Control characters are dropped on arrival, so none of those
// places ever receives them.
func TestControllerNameAndLabelLoseControlCharacters(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	a, err := New(Options{RelayURL: "http://unused", Token: "synthetic", Mode: policy.ModeNormal, Shell: "/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	id := seededIdentity(t, 11)
	pipeHelloAs(t, a, id, "evil\x1b[2J\u009bhost", "who\x1b]0;owned\x07 I am\nsecond line")

	var card console.PendingPairing
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if p := a.console.State().PendingPairings; len(p) == 1 {
			card = p[0]
			break
		}
	}
	if card.Name != "evil[2Jhost" || card.Label != "who]0;owned I am second line" {
		t.Fatalf("pairing card: name %q label %q", card.Name, card.Label)
	}
	events, err := a.log.Read(eventlog.Filter{Type: "connect"})
	if err != nil || len(events) == 0 {
		t.Fatalf("no connect event: %v", err)
	}
	for _, e := range events {
		if strings.ContainsAny(e.PeerName, "\x1b\x07") || strings.Contains(e.PeerName, "\u009b") {
			t.Fatalf("logged peer name kept control characters: %q", e.PeerName)
		}
	}
	// A label that was nothing but control characters is no label: the
	// request is refused as unlabeled rather than shown to the owner blank.
	if a.mustIdentify("hello", "SHA256:x", peerText("\x1b\x07\n")) != true {
		t.Fatal("a label of control characters counted as a self-description")
	}
}

// The device's own approval prompt shows the controller's command. The
// terminal it prints on must not act on what the command contains.
func TestApprovalPromptShowsTheCommandAsText(t *testing.T) {
	got := approvalPrompt(console.Pending{ID: "abc", Cmd: "rm -rf ~ \x1b[1A\x1b[2Kls"})
	if strings.ContainsRune(got, 0x1b) || !strings.Contains(got, `rm -rf ~ \x1b[1A\x1b[2Kls`) {
		t.Fatalf("prompt = %q", got)
	}
}
