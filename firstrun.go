package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"golang.org/x/term"

	"wanctl/internal/config"
)

// First run: a freshly installed binary does not know which instance it talks
// to, and the choice behind that setting — the project's hosted instance, or a
// relay you run yourself — was never put to anyone. On a terminal we ask it
// once and persist the answer through the same code path as `wanctl config
// set`; everywhere else the command fails with both doors named. See GitHub
// issue #11.

// The project's hosted instance. It is invite-only: anyone can sign in with
// GitHub at the portal and ask for access, and an administrator answers
// (GitHub issue #9). Self-hosting is the other door, and neither is a dead end.
const (
	hostedRelay  = "https://wanctl-relay.z10.dev"
	hostedPortal = "https://wanctl.z10.dev"
	selfHostDocs = "https://wc.z10.dev/docs/self-hosting/"
)

// sourceFlag is the pseudo-source for a relay handed to the command on the
// command line. Only `wanctl agent` takes a --relay, and it hands the parsed
// value here so a flag suppresses the question the same way a setting does.
const sourceFlag = "--relay flag"

// firstRunAction is what a relay-needing command should do before it runs.
type firstRunAction int

const (
	// firstRunProceed: a relay is configured and nothing needs saying.
	firstRunProceed firstRunAction = iota
	// firstRunConfirm: the relay was baked into this build, so it is not a
	// question — name it once and carry on.
	firstRunConfirm
	// firstRunAsk: nothing is configured and a human is watching. Ask.
	firstRunAsk
	// firstRunExplain: nothing is configured and nobody can answer. Fail with
	// the exact commands, never block an unattended install on a prompt.
	firstRunExplain
)

// firstRunInput is every fact the decision depends on, so the decision itself
// is a pure function and testable without a terminal.
type firstRunInput struct {
	// Relay is the relay the command already has, and Source where it came
	// from: a flag, an environment variable, the config file, or the build.
	Relay  string
	Source string
	// Interactive is true when stdin and stdout are both a terminal. Stdout
	// matters as much as stdin: a question nobody can read is a hang.
	Interactive bool
	// NoPrompt is the explicit escape hatch (WANCTL_NO_PROMPT).
	NoPrompt bool
	// Enrolled is true once a token exists. It ends the onboarding window, and
	// with it the one-line confirmation of a built-in relay.
	Enrolled bool
	// Android has no shell to run the printed command in, and the app's
	// enrollment dialog is the only place an address can be typed.
	Android bool
}

// decideFirstRun is the whole decision table for issue #11.
func decideFirstRun(in firstRunInput) firstRunAction {
	if in.Relay != "" {
		// Someone already answered. Confirm a build default once during
		// onboarding, because the answer was given by whoever built or served
		// the installer rather than by the person at the keyboard.
		if in.Source == config.SourceBuildDefault && in.Interactive && !in.NoPrompt && !in.Enrolled {
			return firstRunConfirm
		}
		return firstRunProceed
	}
	if in.Android || in.NoPrompt || !in.Interactive {
		return firstRunExplain
	}
	return firstRunAsk
}

// stdioIsTerminal is a variable so tests can decide the answer.
var stdioIsTerminal = func() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// promptSuppressed reads WANCTL_NO_PROMPT. Anything but unset/0/false/no turns
// every first-run question into the printed instruction instead.
func promptSuppressed() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WANCTL_NO_PROMPT"))) {
	case "", "0", "false", "no":
		return false
	}
	return true
}

// firstRunInputNow reads the decision's inputs from this process. flagRelay is
// the value of a command's own --relay, or "" when it has none.
func firstRunInputNow(flagRelay string) firstRunInput {
	relay, source := strings.TrimSpace(flagRelay), sourceFlag
	if relay == "" {
		relay, source = config.Setting("relay")
	}
	return firstRunInput{
		Relay:       relay,
		Source:      source,
		Interactive: stdioIsTerminal(),
		NoPrompt:    promptSuppressed(),
		Enrolled:    config.EnvOr("WANCTL_TOKEN", config.StoredToken()) != "",
		Android:     runtime.GOOS == "android",
	}
}

// ensureRelayConfigured runs the first-run gate for a command that cannot work
// without a relay. It returns nil once one is configured — by this call or
// before it — and an error that names both doors when nobody can be asked.
func ensureRelayConfigured(flagRelay string) error {
	in := firstRunInputNow(flagRelay)
	switch decideFirstRun(in) {
	case firstRunConfirm:
		// stderr: a confirmation is a note about the environment, and stdout
		// belongs to whatever the command was actually asked to produce.
		fmt.Fprintf(os.Stderr, "wanctl: using the relay this build was installed with: %s\n", in.Relay)
		fmt.Fprintf(os.Stderr, "        change it with `wanctl config set relay=... portal=...`\n")
		return nil
	case firstRunAsk:
		return askInstance(bufio.NewReader(os.Stdin), os.Stdout)
	case firstRunExplain:
		if in.Android {
			// The Android app renders this in a dialog and has its own place
			// to type an address, so it keeps the short message it had.
			_, err := config.Relay()
			return err
		}
		return errNoInstanceConfigured()
	}
	return nil
}

// errNoInstanceConfigured is what an unattended run gets: the two doors, and
// the exact command for each. Neither is a dead end — the hosted instance is
// invite-only but takes applications, and self-hosting is documented.
func errNoInstanceConfigured() error {
	return fmt.Errorf(`no relay configured, so wanctl does not know which instance to talk to.
Use the project's hosted instance (invite-only — sign in with GitHub at %s and ask for access):
  wanctl config set relay=%s portal=%s
Or a relay you run yourself (%s):
  wanctl config set relay=https://your-relay portal=https://your-portal`,
		hostedPortal, hostedRelay, hostedPortal, selfHostDocs)
}

// askInstance is the first-run question. It persists the answer through
// configSet — the same validate-then-write path as `wanctl config set` — so
// there is one way settings reach disk.
func askInstance(in *bufio.Reader, out io.Writer) error {
	fmt.Fprintf(out, `wanctl is not pointed at an instance yet. Which relay should it use?

  1) The project's hosted instance — %s
     Invite-only: sign in with GitHub there and ask for access.
  2) A relay you run yourself — %s

`, hostedPortal, selfHostDocs)
	for tries := 0; tries < 3; tries++ {
		fmt.Fprint(out, "Choice [1/2]: ")
		line, err := in.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read choice: %w", err)
		}
		switch strings.TrimSpace(line) {
		case "1":
			if err := configSet([]string{"relay=" + hostedRelay, "portal=" + hostedPortal}); err != nil {
				return err
			}
			fmt.Fprintf(out, "The hosted instance is invite-only: sign in at %s and ask for access.\n", hostedPortal)
			return nil
		case "2":
			return askOwnInstance(in, out)
		case "":
			// An empty line is not a vote for the hosted service.
			return fmt.Errorf("cancelled; configure it later with `wanctl config set relay=... portal=...`")
		default:
			fmt.Fprintln(out, "Answer 1 or 2.")
		}
	}
	return fmt.Errorf("no choice after three tries; configure it with `wanctl config set relay=... portal=...`")
}

// askOwnInstance collects the addresses of a self-hosted deployment. Both are
// asked for: the relay carries the traffic, the portal is where login happens,
// and a self-hosted deployment publishes them separately.
func askOwnInstance(in *bufio.Reader, out io.Writer) error {
	var pending []string
	if _, err := config.Relay(); err != nil {
		v, err := promptSetting(in, out, "relay", "Relay URL (e.g. https://relay.example.com): ")
		if err != nil {
			return err
		}
		pending = append(pending, "relay="+v)
	}
	if _, err := config.Portal(); err != nil {
		v, err := promptSetting(in, out, "portal", "Portal URL (e.g. https://portal.example.com): ")
		if err != nil {
			return err
		}
		pending = append(pending, "portal="+v)
	}
	if len(pending) == 0 {
		return nil
	}
	return configSet(pending)
}
