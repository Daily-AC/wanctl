package main

import (
	"context"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAgentErrorsTheAppKeysOn guards a coupling that crosses a language
// boundary and would otherwise rot in silence.
//
// The Android app supervises `wanctl agent` as a child process. It has no
// channel to that process other than its output, so AgentService.consume()
// decides "retry" versus "stop and tell the user" by matching substrings of
// these messages. Reword one and the app stops recognising a permanent failure:
// a device with no credential, or a rejected token, would respawn the agent
// every few seconds forever — draining the battery while showing the user
// nothing that explains it.
//
// So: reword freely, but keep the marker, or change android/java/.../AgentService.java
// in the same commit.
func TestAgentErrorsTheAppKeysOn(t *testing.T) {
	t.Run("no credential", func(t *testing.T) {
		// Explicit empty --token beats whatever StoredToken() finds on the
		// machine running the test.
		err := cmdAgent(context.Background(), []string{"--relay", "", "--token", ""})
		if err == nil {
			t.Fatal("agent with no relay or token must fail")
		}
		if !strings.Contains(err.Error(), "--token") {
			t.Fatalf("AgentService keys on %q appearing in this error; got %q", "--token", err)
		}
	})

	// The other two markers belong to messages produced deep inside a live
	// relay exchange, so they are asserted against the literals rather than
	// re-derived — the point is that grepping for them finds this test.
	for _, marker := range []string{"rejected token", "registered this device name"} {
		if !strings.Contains(agentFatalMarkers, marker) {
			t.Errorf("marker %q is no longer listed", marker)
		}
	}
}

// agentFatalMarkers exists so the strings the Android service treats as fatal
// appear in this package, where a `grep` from either side lands on the contract
// and on the test that pins it. See internal/agent/agent.go for where they are
// produced.
const agentFatalMarkers = "--token | rejected token | registered this device name | initialize device ID:"

// androidAppSources are the files scripts/build-apk.sh compiles. Listed rather
// than globbed, because a glob over a directory that is not there matches
// nothing and passes.
var androidAppSources = []string{
	"AdbPortWatcher.java",
	"AgentService.java",
	"AgentState.java",
	"ApprovalActivity.java",
	"ApprovalNotifier.java",
	"ApprovalReceiver.java",
	"BootReceiver.java",
	"ConnectionMark.java",
	"DeviceState.java",
	"Installer.java",
	"KeeperJob.java",
	"LogActivity.java",
	"MainActivity.java",
	"Prefs.java",
	"SetupGuide.java",
	"Wanctl.java",
}

// TestAndroidAppIsInTheRepository asserts the app's sources are in the checkout
// this test is running from.
//
// That sounds like it cannot fail, and it did. `.gitignore` opened with a bare
// `wanctl` — meant for the built binary at the root, but a pattern without a
// leading slash matches at every depth, so it also matched
// android/java/dev/wanctl/agent/ and kept all nine files out of the
// repository. Every local build worked, because the files were on disk; the
// acceptance APK was built from a working tree, not from a clone.
//
// Nothing caught it for the same reason it is worth a test: `package:apk` is
// the only job that compiles Java, and it runs on tags only. So the first
// checkout that could notice was the release itself, which failed at
// `find: android/java: No such file or directory` after the signing keys had
// already been handed out.
func TestAndroidAppIsInTheRepository(t *testing.T) {
	for _, name := range androidAppSources {
		path := filepath.Join("android", "java", "dev", "wanctl", "agent", name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s is missing from the checkout: %v\n"+
				"\tif it was deleted on purpose, drop it from androidAppSources;\n"+
				"\tif not, check .gitignore for a pattern with no leading slash", path, err)
		}
	}
}

// TestAppKeysOnMarkersItCanActuallySee closes the other half of
// TestAgentErrorsTheAppKeysOn, which pinned the Go side of the contract and
// took the Java side on trust — so it kept passing while AgentService.java was
// not in the repository at all.
func TestAppKeysOnMarkersItCanActuallySee(t *testing.T) {
	service, err := os.ReadFile(filepath.Join("android", "java", "dev", "wanctl", "agent", "AgentService.java"))
	if err != nil {
		t.Fatalf("read AgentService.java: %v", err)
	}
	for _, marker := range strings.Split(agentFatalMarkers, " | ") {
		if !strings.Contains(string(service), marker) {
			t.Errorf("AgentService.java does not match on %q, so a permanent failure would be retried forever", marker)
		}
	}
}

// TestElevationSwitchReachesThePortWatch pins an ordering inside
// AgentService.onStartCommand that nothing on the Go side can observe.
//
// Flipping 提权通道 while the agent runs reaches the service as ACTION_RESTART,
// and that branch returns as soon as it has killed the child. The call that
// starts or stops the wireless-debugging port watch sat below it from the day
// discovery was written, so it never ran for the one change that decides it:
// the respawned agent had elevation and a state file with no port in it, and
// told a phone whose wireless debugging was on to turn wireless debugging on,
// until something recreated the service. Switching the channel off left the
// watch and its MulticastLock running the same way.
func TestElevationSwitchReachesThePortWatch(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("android", "java", "dev", "wanctl", "agent", "AgentService.java"))
	if err != nil {
		t.Fatalf("read AgentService.java: %v", err)
	}
	body := javaMethodBody(t, string(src), "public int onStartCommand(")
	refresh := strings.Index(body, "deviceState.refreshAdbPortWatch();")
	restart := strings.Index(body, "ACTION_RESTART.equals(action)")
	if refresh < 0 {
		t.Fatal("onStartCommand no longer refreshes the adb port watch, so 提权通道 would only take effect when the service is recreated")
	}
	if restart < 0 {
		t.Fatal("onStartCommand has no ACTION_RESTART branch any more; check how a settings change reaches the port watch, then update this test")
	}
	if refresh > restart {
		t.Error("onStartCommand refreshes the adb port watch after the ACTION_RESTART branch, which returns early: " +
			"switching 提权通道 on a running agent would not start the watch")
	}
}

// javaMethodBody returns the source of the method whose declaration starts with
// signature, up to its closing brace. The app's sources are indented four
// spaces per level, so a method ends at the first line that is a brace at
// member depth.
func javaMethodBody(t *testing.T, src, signature string) string {
	t.Helper()
	start := strings.Index(src, signature)
	if start < 0 {
		t.Fatalf("no method %q", signature)
	}
	end := strings.Index(src[start:], "\n    }\n")
	if end < 0 {
		t.Fatalf("method %q has no closing brace at member depth", signature)
	}
	return src[start : start+end]
}

// TestApprovalCardsStayOutOfTheAppLog pins the app's half of the stdio link in
// ADR 0015. With --approvals-stdio the agent prints each approval card as one
// line starting with approvalLinePrefix (internal/agent/approvals.go). A card
// carries the text of a command someone else wants to run, so
// AgentService.consume must take such a line before append(), which writes the
// log ring, agent.log and logcat: the log screen shows all three and copies
// them with one tap.
func TestApprovalCardsStayOutOfTheAppLog(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("android", "java", "dev", "wanctl", "agent", "AgentService.java"))
	if err != nil {
		t.Fatalf("read AgentService.java: %v", err)
	}
	if !strings.Contains(string(src), `APPROVAL_LINE = "wanctl-approval "`) {
		t.Error("AgentService.java no longer matches the agent's approvalLinePrefix, so approval cards would land in the log instead of a notification")
	}
	if !strings.Contains(string(src), `"--approvals-stdio"`) {
		t.Error("AgentService.java no longer starts the agent with --approvals-stdio, so the portal cannot designate this phone")
	}
	body := javaMethodBody(t, string(src), "private void consume(")
	card := strings.Index(body, "startsWith(APPROVAL_LINE)")
	logged := strings.Index(body, "append(line)")
	if card < 0 || logged < 0 {
		t.Fatal("consume() no longer has both the approval-card branch and append(line); check where cards go, then update this test")
	}
	if card > logged {
		t.Error("consume() logs a line before checking whether it is an approval card: the command text would reach agent.log")
	}
}

// TestApprovalDetailWaitsForTheUnlock pins "unlock is required" (ADR 0015,
// decision 1) on the app side: the screen that shows a card's command is never
// allowed over the keyguard, so tapping the notification on a locked phone makes
// Android ask for the unlock first. Neither it nor the receiver behind the
// notification's buttons may be reachable from another app.
func TestApprovalDetailWaitsForTheUnlock(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("android", "AndroidManifest.xml"))
	if err != nil {
		t.Fatalf("read AndroidManifest.xml: %v", err)
	}
	type component struct {
		Name           string `xml:"http://schemas.android.com/apk/res/android name,attr"`
		Exported       string `xml:"http://schemas.android.com/apk/res/android exported,attr"`
		ShowWhenLocked string `xml:"http://schemas.android.com/apk/res/android showWhenLocked,attr"`
		TurnScreenOn   string `xml:"http://schemas.android.com/apk/res/android turnScreenOn,attr"`
	}
	var m struct {
		Activities []component `xml:"application>activity"`
		Receivers  []component `xml:"application>receiver"`
	}
	if err := xml.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse AndroidManifest.xml: %v", err)
	}
	find := func(list []component, name string) *component {
		for i := range list {
			if list[i].Name == name {
				return &list[i]
			}
		}
		t.Fatalf("%s is not declared in AndroidManifest.xml", name)
		return nil
	}
	detail := find(m.Activities, ".ApprovalActivity")
	if detail.Exported != "false" {
		t.Errorf("ApprovalActivity must be exported=\"false\", got %q", detail.Exported)
	}
	if detail.ShowWhenLocked == "true" || detail.TurnScreenOn == "true" {
		t.Error("ApprovalActivity may not show over the keyguard: the command must wait for the unlock")
	}
	if r := find(m.Receivers, ".ApprovalReceiver"); r.Exported != "false" {
		t.Errorf("ApprovalReceiver must be exported=\"false\", got %q", r.Exported)
	}
	src, err := os.ReadFile(filepath.Join("android", "java", "dev", "wanctl", "agent", "ApprovalActivity.java"))
	if err != nil {
		t.Fatalf("read ApprovalActivity.java: %v", err)
	}
	for _, call := range []string{"setShowWhenLocked", "setTurnScreenOn", "FLAG_SHOW_WHEN_LOCKED", "requestDismissKeyguard"} {
		if strings.Contains(string(src), call) {
			t.Errorf("ApprovalActivity.java calls %s: the command must wait for the unlock", call)
		}
	}
}
