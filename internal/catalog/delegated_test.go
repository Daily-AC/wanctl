package catalog

import (
	"strings"
	"testing"
)

// The point of rendering a delegated client's instructions from this package is
// that the two surfaces cannot drift apart unnoticed. What makes that true is
// this test: every rule the delegated form restates quotes the catalog rule it
// came from, and a reworded rule upstream fails here until someone decides what
// the delegated client should now be told.
func TestDelegatedRulesQuoteTheCatalogVerbatim(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range delegatedRules {
		found := false
		for _, rule := range devLoop {
			if rule == r.Source {
				found = true
			}
		}
		if !found {
			t.Errorf("no DEV LOOP rule reads %q any more; re-decide its delegated form", r.Source)
		}
		if seen[r.Source] {
			t.Errorf("rule restated twice: %q", r.Source)
		}
		seen[r.Source] = true
	}
	for _, rule := range devLoop {
		if !seen[rule] {
			t.Errorf("DEV LOOP rule has no delegated form: %q", rule)
		}
	}
}

// Same contract for a primitive's one-line description: three of the four are
// the catalog's own, and the fourth says out loud which line it replaces.
func TestDelegatedToolsTrackTheirCatalogCommands(t *testing.T) {
	for _, tool := range delegatedTools {
		c, ok := Lookup(tool.MCPName)
		if !ok {
			t.Errorf("%s maps to %s, which is not a command", tool.Name, tool.MCPName)
			continue
		}
		if tool.Instead == "" {
			if tool.Line != "" {
				t.Errorf("%s overrides the catalog without saying what it replaces", tool.Name)
			}
			continue
		}
		if c.IndexLine != tool.Instead {
			t.Errorf("%s no longer reads %q but %q; re-decide the delegated line", tool.MCPName, tool.Instead, c.IndexLine)
		}
	}
}

// What a delegated client must be able to read, and what it must never be
// offered: a primitive it does not have is a call it will waste a turn on.
func TestDelegatedInstructionsCarryOnlyDelegatedPrimitives(t *testing.T) {
	got := DelegatedInstructions()
	for _, want := range append(DelegatedTools(),
		Headline, "AGENTS.md", "CLAUDE.md", "and follow it", "Never cat/sed/echo a file through a shell") {
		if !strings.Contains(got, want) {
			t.Errorf("delegated instructions no longer say %q:\n%s", want, got)
		}
	}
	for _, absent := range []string{"wanctl_", "exec_async", "exec_poll", "push", "pull", "screenshot", "persistent shell", "LOGIN REQUIRED"} {
		if strings.Contains(got, absent) {
			t.Errorf("delegated instructions offer %q, which a delegated session does not have:\n%s", absent, got)
		}
	}
	for i, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if n := len([]rune(line)); n > Width {
			t.Errorf("line %d is %d columns: %q", i+1, n, line)
		}
	}
}
