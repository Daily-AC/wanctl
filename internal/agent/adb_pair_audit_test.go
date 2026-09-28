package agent

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"wanctl/internal/eventlog"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
)

// An adb-pair run through exec is recorded — command, outcome, the
// notification about it — without the pairing code in any of them.
func TestADBPairCodeStaysOutOfTheRecord(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	a, err := New(Options{RelayURL: "https://relay.example", Token: "token", Mode: policy.ModeBypass, Shell: "/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	a.notifyPolicy.IncludeDetail = true
	reports := make(chan string, 8)
	a.notifyClient = &http.Client{Transport: notifyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		reports <- string(body)
		return notifyHTTPResponse(http.StatusAccepted), nil
	})}

	conn := pipeController(t, a, 3)
	roundTrip(t, conn, protocol.Message{Kind: protocol.KindExec, Command: "adb-pair 37123 482913", OneShot: true})

	select {
	case body := <-reports:
		if strings.Contains(body, "482913") || !strings.Contains(body, "adb-pair 37123 [redacted]") {
			t.Fatalf("exec notification: %s", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no exec notification")
	}
	raw := readLog(t, a)
	if strings.Contains(raw, "482913") || !strings.Contains(raw, "adb-pair 37123 [redacted]") {
		t.Fatalf("event log: %s", raw)
	}
}

// Pairing started from the portal console changes what this device's adbd
// trusts, so it leaves an event too — with the port and the outcome, and
// without the code.
func TestConsoleADBPairIsRecordedWithoutTheCode(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	a, err := New(Options{RelayURL: "https://relay.example", Token: "token", Shell: "/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	a.handleConsoleRPC(protocol.Message{Kind: protocol.KindADBPair, PairPort: 37129, PairCode: "012345"})
	events, err := a.log.Read(eventlog.Filter{Grep: "adb-pair"})
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %+v, %v; want one record of the pairing", events, err)
	}
	e := events[0]
	if e.Detail != "adb-pair 37129 [redacted]" || e.Decision != "console" || e.Exit == nil {
		t.Fatalf("event = %+v", e)
	}
	if raw := readLog(t, a); strings.Contains(raw, "012345") {
		t.Fatalf("the code reached the log: %s", raw)
	}
}

func readLog(t *testing.T, a *Agent) string {
	t.Helper()
	events, err := a.log.Read(eventlog.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range events {
		b.WriteString(e.Detail + " " + e.Cwd + " " + e.PeerName + "\n")
	}
	return b.String()
}
