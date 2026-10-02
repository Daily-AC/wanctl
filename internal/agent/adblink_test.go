package agent

import (
	"bytes"
	"strings"
	"testing"

	"wanctl/internal/console"
)

// The app learns the adb link state from one stdout line per change, and the
// portal reads the same state from the console snapshot.
func TestADBLinkReachesTheAppOncePerChange(t *testing.T) {
	var out bytes.Buffer
	a := &Agent{phone: newApprovalPhone(&out, strings.NewReader(""), nil)}
	if a.adbLink() != nil {
		t.Fatal("a link state before any probe")
	}
	a.setADBLink(console.ADBLink{State: "no_port", Reason: "nothing on 5555"})
	a.setADBLink(console.ADBLink{State: "no_port", Reason: "nothing on 5555"})
	a.setADBLink(console.ADBLink{State: "connected"})
	want := "wanctl-adb {\"state\":\"no_port\"}\nwanctl-adb {\"state\":\"connected\"}\n"
	if out.String() != want {
		t.Fatalf("stdout = %q, want %q", out.String(), want)
	}
	if l := a.adbLink(); l == nil || l.State != "connected" {
		t.Fatalf("snapshot link = %+v", l)
	}
	a.kickADBLink() // no watcher and no channel: must not block
}
