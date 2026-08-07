package main

import (
	"context"
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
const agentFatalMarkers = "--token | rejected token | registered this device name"

// androidAppSources are the files scripts/build-apk.sh compiles. Listed rather
// than globbed, because a glob over a directory that is not there matches
// nothing and passes.
var androidAppSources = []string{
	"AgentService.java",
	"AgentState.java",
	"BootReceiver.java",
	"Installer.java",
	"KeeperJob.java",
	"LogActivity.java",
	"MainActivity.java",
	"Prefs.java",
	"Wanctl.java",
}

// TestAndroidAppIsInTheRepository asserts the app's sources are in the checkout
// this test is running from.
//
// That sounds like it cannot fail, and it did. `.gitignore` opened with a bare
// `wanctl` — meant for the built binary at the root, but a pattern without a
// leading slash matches at every depth, so it also matched
// android/java/com/***REMOVED***/wanctl/ and kept all nine files out of the
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
		path := filepath.Join("android", "java", "com", "***REMOVED***", "wanctl", name)
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
	service, err := os.ReadFile(filepath.Join("android", "java", "com", "***REMOVED***", "wanctl", "AgentService.java"))
	if err != nil {
		t.Fatalf("read AgentService.java: %v", err)
	}
	for _, marker := range strings.Split(agentFatalMarkers, " | ") {
		if !strings.Contains(string(service), marker) {
			t.Errorf("AgentService.java does not match on %q, so a permanent failure would be retried forever", marker)
		}
	}
}
