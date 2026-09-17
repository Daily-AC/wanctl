package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"wanctl/internal/catalog"
)

// buildWanctl builds the binary once per test that needs to run real commands.
func buildWanctl(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := filepath.Join(t.TempDir(), "wanctl")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

// runWanctl runs the binary with a throwaway config dir and no relay, so help
// never reaches the first-run question or writes anything.
func runWanctl(t *testing.T, bin string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(),
		"WANCTL_CONFIG_DIR="+filepath.Join(t.TempDir(), "config"),
		"WANCTL_RELAY=", "WANCTL_TOKEN=")
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run %v: %v\n%s", args, err, out)
		}
		code = exitErr.ExitCode()
	}
	return string(out), code
}

// The index exists to be read. Sixty lines of flags at the entry point is the
// same as printing nothing, so the budget is part of the contract: thirty lines
// that fit an eighty-column terminal, whole commands explained elsewhere.
func TestIndexFitsTheBudget(t *testing.T) {
	bin := buildWanctl(t)
	out, code := runWanctl(t, bin)
	if code != 0 {
		t.Fatalf("bare wanctl exited %d\n%s", code, out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) > 30 {
		t.Errorf("index is %d lines, budget is 30:\n%s", len(lines), out)
	}
	for i, line := range lines {
		if n := len([]rune(line)); n > 80 {
			t.Errorf("line %d is %d columns: %q", i+1, n, line)
		}
	}
	// The status line is what makes bare `wanctl` worth running at all; it
	// answers "is this machine logged in, is the agent up" without a flag.
	if !strings.Contains(out, "本机:") {
		t.Errorf("index lost the local status line:\n%s", out)
	}
}

// Every command in the index has an entry, and every entry renders. A command
// listed but undocumented is worse than one that is neither.
func TestEveryIndexedCommandHasAnEntry(t *testing.T) {
	for _, c := range catalog.Commands {
		if c.Summary == "" {
			t.Errorf("%s%s has no summary", c.Name, c.MCPName)
		}
		if c.Desc == "" {
			t.Errorf("%s%s has no description", c.Name, c.MCPName)
		}
		if c.Name != "" && c.CLIExample == "" {
			t.Errorf("%s has no CLI example", c.Name)
		}
		if c.MCPName != "" && c.MCPExample == "" {
			t.Errorf("%s has no MCP example", c.MCPName)
		}
	}
}

// Both spellings resolve. An agent that only ever saw the MCP tool list knows
// the name `wanctl_read`, and `wanctl help wanctl_read` has to answer rather
// than send it away.
func TestHelpRendersEntriesForBothSpellings(t *testing.T) {
	bin := buildWanctl(t)
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"help", "exec"}, []string{"wanctl exec —", "wanctl_exec", "PARAMETERS", "--script", "ERRORS", "PAIRING REQUIRED"}},
		{[]string{"exec", "-h"}, []string{"wanctl exec —", "--oneshot", "EXAMPLE"}},
		{[]string{"help", "read"}, []string{"wanctl read —", "wanctl_read", "--offset N"}},
		{[]string{"help", "wanctl_read"}, []string{"wanctl read —", "wanctl_read"}},
		{[]string{"help", "trust", "server"}, []string{"wanctl trust server —", "--fingerprint"}},
		{[]string{"help", "start"}, []string{"wanctl start —", "CLI only"}},
		{[]string{"help", "write"}, []string{"wanctl write —", "wanctl_write", "--content-file F"}},
		{[]string{"help", "wanctl_screenshot"}, []string{"wanctl screenshot —", "wanctl_screenshot"}},
	} {
		out, code := runWanctl(t, bin, tc.args...)
		if code != 0 {
			t.Errorf("wanctl %v exited %d\n%s", tc.args, code, out)
			continue
		}
		for _, want := range tc.want {
			if !strings.Contains(out, want) {
				t.Errorf("wanctl %v output does not contain %q:\n%s", tc.args, want, out)
			}
		}
	}
}

// A typo in a script must fail, not look like it documented something.
func TestHelpForUnknownCommandFails(t *testing.T) {
	bin := buildWanctl(t)
	out, code := runWanctl(t, bin, "help", "nosuch")
	if code == 0 {
		t.Errorf("wanctl help nosuch succeeded:\n%s", out)
	}
	if !strings.Contains(out, "nosuch") || !strings.Contains(out, "USAGE") {
		t.Errorf("the failure does not name the typo and show the index:\n%s", out)
	}
}

// The README makes the same claim as the index header and the contract intro.
// It is hand-written, so the most a test can do is refuse to let it drift away
// from the one sentence the other two render.
func TestReadmeCarriesTheHeadline(t *testing.T) {
	b, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), catalog.Headline) {
		t.Errorf("README.md no longer says %q; the index header and docs/contract.md do", catalog.Headline)
	}
}

// docs/contract.md is the catalog, committed. It is what a reader outside a
// terminal gets, and a stale copy is a contract that lies.
func TestContractDocIsInSync(t *testing.T) {
	const path = "docs/contract.md"
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != catalog.Markdown() {
		t.Errorf("%s is out of date. Regenerate it:\n\n    go run . help --markdown > %s\n", path, path)
	}
}

// `wanctl help --instructions` prints what an MCP host is handed before it calls
// anything. It exists so the same words can be read by something that is not an
// MCP client — a discovery page, or a person deciding what this will do to their
// machine — without a second copy of them existing anywhere.
func TestHelpPrintsTheInstructions(t *testing.T) {
	bin := buildWanctl(t)
	out, code := runWanctl(t, bin, "help", "--instructions")
	if code != 0 {
		t.Fatalf("help --instructions exited %d\n%s", code, out)
	}
	if out != catalog.Instructions() {
		t.Errorf("printed instructions differ from the catalog's:\n%s", out)
	}
	if n := len(strings.Split(strings.TrimRight(out, "\n"), "\n")); n > 40 {
		t.Errorf("instructions are %d lines, budget is 40", n)
	}
	for _, want := range append([]string{catalog.Headline, "AGENTS.md", "wanctl_write", "wanctl_screenshot"},
		catalog.CriticalErrors()...) {
		if !strings.Contains(out, want) {
			t.Errorf("instructions do not mention %q", want)
		}
	}
}
