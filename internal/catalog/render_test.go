package catalog

import (
	"strings"
	"testing"
)

// Every entry is read in a terminal, so every entry has to fit one. Wrapping is
// the renderer's job for prose; an example or a parameter spelling that does
// not fit is the catalog's own fault and is fixed by shortening it.
func TestEntriesFitEightyColumns(t *testing.T) {
	for _, c := range Commands {
		for i, line := range strings.Split(Entry(c), "\n") {
			if n := len([]rune(line)); n > Width {
				t.Errorf("%s%s entry line %d is %d columns: %q", c.Name, c.MCPName, i+1, n, line)
			}
		}
	}
}

// The index is what bare `wanctl` prints. Its budget is asserted against the
// real binary in the main package too; this catches it without a build.
func TestIndexFitsItsBudget(t *testing.T) {
	out := Index("https://relay.example.com", "https://portal.example.com")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// One line is left for the local status line the CLI appends.
	if len(lines) > 29 {
		t.Errorf("index is %d lines, leaving no room for the status line:\n%s", len(lines), out)
	}
	for i, line := range lines {
		if n := len([]rune(line)); n > Width {
			t.Errorf("index line %d is %d columns: %q", i+1, n, line)
		}
	}
}

// The index header and the contract intro are the same claim. They render from
// Headline so that changing what wanctl is cannot leave one of them behind.
func TestIndexHeaderRendersFromProduct(t *testing.T) {
	if !strings.Contains(Index("", ""), Headline) {
		t.Errorf("the index header does not render from Headline:\n%s", Index("", ""))
	}
	if !strings.HasPrefix(Product, "wanctl is "+Headline) {
		t.Errorf("Product does not open with Headline: %q", Product)
	}
}

// A long self-hosted URL must wrap rather than overflow.
func TestDefaultsWrapRatherThanOverflow(t *testing.T) {
	long := "https://" + strings.Repeat("a", 60) + ".example.com"
	for _, line := range defaultsLines(long, long) {
		if n := len([]rune(line)); n > Width {
			t.Errorf("defaults line is %d columns: %q", n, line)
		}
	}
}

// Both spellings resolve, including the multi-word CLI names.
func TestLookupResolvesBothSpellings(t *testing.T) {
	for _, name := range []string{"exec", "wanctl_exec", "read", "wanctl_read", "trust server", "wanctl_trust_server", "start", "help"} {
		if _, ok := Lookup(name); !ok {
			t.Errorf("Lookup(%q) found nothing", name)
		}
	}
	if _, ok := Lookup("nosuch"); ok {
		t.Error("Lookup invented a command")
	}
}

// Every command the index names has an entry, and every entry the index does
// not name is still reachable through MORE or a group. A command that appears
// nowhere is a command nobody finds.
func TestIndexNamesEveryCLICommand(t *testing.T) {
	index := Index("", "")
	for _, c := range Commands {
		if c.Name == "" || strings.Contains(c.Name, " ") || c.Name == "help" {
			continue
		}
		if !strings.Contains(index, " "+c.Name+" ") && !strings.Contains(index, " "+c.Name+"\n") {
			t.Errorf("the index never names %q", c.Name)
		}
	}
}

// The contract opens with what wanctl is, because a command list does not say
// what the list is a list of. The brain/hands framing is the owner's, and it is
// what tells a reader why the primitives stop where they do.
func TestMarkdownCarriesTheProductDefinition(t *testing.T) {
	// The intro is wrapped for the page, so a phrase can straddle two lines.
	// Compare against the unwrapped text.
	md := strings.Join(strings.Fields(Markdown()), " ")
	for _, want := range []string{Headline, "Together they form one agent", "no browser driver", "wanctl help --markdown"} {
		if !strings.Contains(md, want) {
			t.Errorf("the Markdown contract does not say %q", want)
		}
	}
}

// An array parameter's element shape is read from the schema the tool actually
// registers. Hardcoding "{old, new}" was true only while `edits` was the only
// array in the catalog, and would have quietly mislabelled the next one.
func TestArrayParameterRendersItsDeclaredItemShape(t *testing.T) {
	objects := Param{Name: "edits", Type: TypeArray, Items: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"new": map[string]any{"type": "string"},
			"old": map[string]any{"type": "string"},
		},
		"required": []string{"old", "new"},
	}}
	if got := typeLabel(objects); got != "array of {old, new}" {
		t.Errorf("label = %q, want the fields in their declared order", got)
	}

	// Another shape must not come out claiming to be pairs of old and new.
	for _, tc := range []struct {
		name string
		p    Param
		want string
	}{
		{"strings", Param{Type: TypeArray, Items: map[string]any{"type": "string"}}, "array of string values"},
		{"unspecified", Param{Type: TypeArray}, "array of values"},
		{"an object described in JSON", Param{Type: TypeArray, Items: map[string]any{
			"type":       "object",
			"properties": map[string]any{"path": map[string]any{}, "mode": map[string]any{}},
			"required":   []any{"path"},
		}}, "array of {path, mode}"},
		{"not an array", Param{Type: TypeString}, "string"},
	} {
		if got := typeLabel(tc.p); got != tc.want {
			t.Errorf("%s: label = %q, want %q", tc.name, got, tc.want)
		}
	}

	// And the rendering the contract prints comes from the same function.
	edit, ok := Lookup("wanctl_edit")
	if !ok {
		t.Fatal("wanctl_edit is not in the catalog")
	}
	md := Markdown()
	for _, p := range edit.Params {
		if p.Type == TypeArray && !strings.Contains(md, typeLabel(p)) {
			t.Errorf("the contract does not print %q for %s", typeLabel(p), p.Name)
		}
	}
}
