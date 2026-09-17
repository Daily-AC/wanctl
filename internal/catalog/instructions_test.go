package catalog

import (
	"strings"
	"testing"
)

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

// What they have to contain: the claim, every primitive by name, the rule about
// a project's own instructions, and the four refusals a caller matches on.
func TestInstructionsCarryTheContract(t *testing.T) {
	got := Instructions()
	if !strings.Contains(got, Headline) {
		t.Errorf("instructions do not carry the headline %q", Headline)
	}
	for _, c := range MCPCommands() {
		if !strings.Contains(got, c.MCPName) {
			t.Errorf("instructions never mention %s", c.MCPName)
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
