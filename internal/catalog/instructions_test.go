package catalog

import (
	"regexp"
	"strings"
	"testing"
)

var toolNames = regexp.MustCompile(`wanctl_[a-z_]+`)

// The instructions are read before any work, every session, by something that
// pays for every token. Forty lines is the budget; eighty columns is the same
// width the rest of the contract is written to.
func TestInstructionsFitTheBudget(t *testing.T) {
	lines := strings.Split(strings.TrimRight(Instructions(), "\n"), "\n")
	if len(lines) > 40 {
		t.Errorf("instructions are %d lines, budget is 40:\n%s", len(lines), Instructions())
	}
	for i, line := range lines {
		if n := len([]rune(line)); n > Width {
			t.Errorf("line %d is %d columns: %q", i+1, n, line)
		}
	}
}

// What they have to contain: the claim, the rule about a project's own
// instructions, and the refusals a caller matches on. Not the tool list: the
// host already has every name and description, and a second copy here was
// most of what this text cost. A tool it does name has to exist.
func TestInstructionsCarryTheContract(t *testing.T) {
	got := Instructions()
	if !strings.Contains(got, Headline) {
		t.Errorf("instructions do not carry the headline %q", Headline)
	}
	for _, name := range toolNames.FindAllString(got, -1) {
		if c, ok := Lookup(name); !ok || c.MCPName != name {
			t.Errorf("instructions name %s, which is not a tool", name)
		}
	}
	for _, phrase := range []string{"AGENTS.md", "CLAUDE.md", "follow it"} {
		if !strings.Contains(got, phrase) {
			t.Errorf("instructions no longer say %q", phrase)
		}
	}
	for _, text := range CriticalErrors() {
		if !strings.Contains(got, text) {
			t.Errorf("instructions no longer quote %q", text)
		}
	}
}

// An error advertised here that no command actually returns would send a caller
// waiting for a string it will never see.
func TestEveryQuotedRefusalIsRealOne(t *testing.T) {
	for _, text := range CriticalErrors() {
		if !Declares(text) {
			t.Errorf("the instructions quote %q but no command declares it", text)
		}
	}
}

// The hosted endpoint's text is the same contract less the local login: its
// requests are authenticated by an OAuth bearer before any tool runs, so a
// model told to log in there would go looking for a tool that is not offered.
func TestHostedInstructionsHaveNoLogin(t *testing.T) {
	got := HostedInstructions()
	for _, stale := range []string{"wanctl_login", "LOGIN REQUIRED", "rebind", "one-time code"} {
		if strings.Contains(got, stale) {
			t.Errorf("hosted instructions mention %q:\n%s", stale, got)
		}
	}
	for _, name := range toolNames.FindAllString(got, -1) {
		if c, ok := Lookup(name); !ok || c.MCPName != name || c.StdioOnly {
			t.Errorf("hosted instructions name %s, which the hosted endpoint does not offer", name)
		}
	}
	if !strings.Contains(Instructions(), "wanctl_login") {
		t.Error("the local instructions lost wanctl_login")
	}
}
