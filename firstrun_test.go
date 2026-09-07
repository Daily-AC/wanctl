package main

import (
	"bufio"
	"io"
	"strings"
	"testing"

	"wanctl/internal/config"
)

// The decision table from GitHub issue #11. Every row is a way the question
// must NOT be asked, plus the one way it must be.
func TestDecideFirstRun(t *testing.T) {
	tty := func(in firstRunInput) firstRunInput { in.Interactive = true; return in }
	for _, tc := range []struct {
		name string
		in   firstRunInput
		want firstRunAction
	}{
		{"configured in the config file", tty(firstRunInput{Relay: "https://r.example", Source: "config file"}), firstRunProceed},
		{"configured by the environment", tty(firstRunInput{Relay: "https://r.example", Source: "env WANCTL_RELAY"}), firstRunProceed},
		{"given on the command line", tty(firstRunInput{Relay: "wss://r.example", Source: sourceFlag}), firstRunProceed},
		{"baked into the build, still onboarding", tty(firstRunInput{Relay: "https://r.example", Source: config.SourceBuildDefault}), firstRunConfirm},
		{"baked into the build, already enrolled", tty(firstRunInput{Relay: "https://r.example", Source: config.SourceBuildDefault, Enrolled: true}), firstRunProceed},
		{"baked into the build, no terminal", firstRunInput{Relay: "https://r.example", Source: config.SourceBuildDefault}, firstRunProceed},
		{"baked into the build, prompts refused", tty(firstRunInput{Relay: "https://r.example", Source: config.SourceBuildDefault, NoPrompt: true}), firstRunProceed},
		{"nothing configured, terminal", tty(firstRunInput{}), firstRunAsk},
		{"nothing configured, no terminal", firstRunInput{}, firstRunExplain},
		{"nothing configured, prompts refused", tty(firstRunInput{NoPrompt: true}), firstRunExplain},
		{"nothing configured, android", tty(firstRunInput{Android: true}), firstRunExplain},
	} {
		if got := decideFirstRun(tc.in); got != tc.want {
			t.Errorf("%s: decideFirstRun = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestPromptSuppressed(t *testing.T) {
	for _, v := range []string{"", "0", "false", "no", " NO "} {
		t.Setenv("WANCTL_NO_PROMPT", v)
		if promptSuppressed() {
			t.Errorf("WANCTL_NO_PROMPT=%q suppressed the prompt", v)
		}
	}
	for _, v := range []string{"1", "true", "yes", "anything"} {
		t.Setenv("WANCTL_NO_PROMPT", v)
		if !promptSuppressed() {
			t.Errorf("WANCTL_NO_PROMPT=%q did not suppress the prompt", v)
		}
	}
}

// fakeTerminal decides the answer the first-run gate gets for "is a human
// watching", which is otherwise a real file descriptor.
func fakeTerminal(t *testing.T, on bool) func() {
	t.Helper()
	old := stdioIsTerminal
	stdioIsTerminal = func() bool { return on }
	return func() { stdioIsTerminal = old }
}

// freshConfig isolates the settings directory and clears the environment
// layers above it, so a test starts from a genuinely unconfigured binary.
func freshConfig(t *testing.T) {
	t.Helper()
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", "")
	t.Setenv("WANCTL_PORTAL", "")
	t.Setenv("WANCTL_TOKEN", "")
	t.Setenv("WANCTL_NO_PROMPT", "")
}

func TestAskInstanceHosted(t *testing.T) {
	freshConfig(t)
	if err := askInstance(bufio.NewReader(strings.NewReader("1\n")), io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := config.StoredSetting("relay"); got != hostedRelay {
		t.Fatalf("relay = %q, want %q", got, hostedRelay)
	}
	if got := config.StoredSetting("portal"); got != hostedPortal {
		t.Fatalf("portal = %q, want %q", got, hostedPortal)
	}
}

func TestAskInstanceOwn(t *testing.T) {
	freshConfig(t)
	in := bufio.NewReader(strings.NewReader("2\nhttps://relay.mine\nhttps://portal.mine\n"))
	if err := askInstance(in, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := config.StoredSetting("relay"); got != "https://relay.mine" {
		t.Fatalf("relay = %q", got)
	}
	if got := config.StoredSetting("portal"); got != "https://portal.mine" {
		t.Fatalf("portal = %q", got)
	}
}

// A typo is re-asked rather than saved: everything a first run persists goes
// through the same validation as `wanctl config set`.
func TestAskInstanceRejectsBadURLThenAccepts(t *testing.T) {
	freshConfig(t)
	out := &strings.Builder{}
	in := bufio.NewReader(strings.NewReader("2\nrelay.mine\nftp://relay.mine\nhttps://relay.mine\nhttps://portal.mine\n"))
	if err := askInstance(in, out); err != nil {
		t.Fatal(err)
	}
	if got := config.StoredSetting("relay"); got != "https://relay.mine" {
		t.Fatalf("relay = %q", got)
	}
	if !strings.Contains(out.String(), "not a URL") {
		t.Fatalf("the bad URL was not reported back: %q", out.String())
	}
}

// Neither an unreadable answer nor a bare Enter may opt someone into a hosted
// service they never chose.
func TestAskInstanceRefusesToGuess(t *testing.T) {
	for _, input := range []string{"\n", "3\n4\n5\n", ""} {
		freshConfig(t)
		err := askInstance(bufio.NewReader(strings.NewReader(input)), io.Discard)
		if err == nil {
			t.Fatalf("input %q was accepted as an answer", input)
		}
		if got := config.StoredSetting("relay"); got != "" {
			t.Fatalf("input %q stored relay %q", input, got)
		}
	}
}

// Unattended: no question, and an error that names both doors and the exact
// command for each.
func TestEnsureRelayConfiguredExplains(t *testing.T) {
	freshConfig(t)
	restore := fakeTerminal(t, false)
	defer restore()

	err := ensureRelayConfigured("")
	if err == nil {
		t.Fatal("an unconfigured non-interactive run was allowed to proceed")
	}
	for _, want := range []string{"wanctl config set relay=", hostedRelay, hostedPortal, selfHostDocs} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// A relay from anywhere — flag, environment, config file, build — settles the
// question, and none of these may block.
func TestEnsureRelayConfiguredAcceptsEveryConfiguredSource(t *testing.T) {
	freshConfig(t)
	defer fakeTerminal(t, true)()

	if err := ensureRelayConfigured("wss://flag.example"); err != nil {
		t.Fatalf("--relay rejected: %v", err)
	}
	t.Setenv("WANCTL_RELAY", "https://env.example")
	if err := ensureRelayConfigured(""); err != nil {
		t.Fatalf("WANCTL_RELAY rejected: %v", err)
	}
	t.Setenv("WANCTL_RELAY", "")
	if err := configSet([]string{"relay=https://file.example"}); err != nil {
		t.Fatal(err)
	}
	if err := ensureRelayConfigured(""); err != nil {
		t.Fatalf("stored relay rejected: %v", err)
	}
}

// A build that carries a relay — a relay-served installer, an enterprise build
// — has already answered. Confirm, never re-ask.
func TestEnsureRelayConfiguredConfirmsBuildDefault(t *testing.T) {
	freshConfig(t)
	defer fakeTerminal(t, true)()
	old := config.DefaultRelay
	config.DefaultRelay = "https://baked.example"
	t.Cleanup(func() { config.DefaultRelay = old })

	if err := ensureRelayConfigured(""); err != nil {
		t.Fatalf("baked-in relay rejected: %v", err)
	}
	if got := config.StoredSetting("relay"); got != "" {
		t.Fatalf("the confirmation persisted %q; a build default must stay a build default", got)
	}
}
