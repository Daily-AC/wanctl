package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wanctl/internal/catalog"
)

// --old and --old-file are two ways to say the same thing, and they disagree
// when both are given. Picking one silently is how the wrong text ends up in
// somebody's file.
func TestEditTextSources(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "block.txt")
	const fromFile = "line one\nline two\n"
	if err := os.WriteFile(path, []byte(fromFile), 0o644); err != nil {
		t.Fatal(err)
	}

	if got, err := editText("old", "inline", ""); err != nil || got != "inline" {
		t.Fatalf("inline = %q, err = %v", got, err)
	}
	if got, err := editText("old", "", path); err != nil || got != fromFile {
		t.Fatalf("from file = %q, err = %v", got, err)
	}
	if got, err := editText("new", "", ""); err != nil || got != "" {
		t.Fatalf("empty replacement = %q, err = %v", got, err)
	}
	if _, err := editText("old", "inline", path); err == nil {
		t.Fatal("giving both -old and -old-file was accepted")
	}
	if _, err := editText("old", "", filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing -old-file was accepted")
	}
}

// The two new verbs are reachable and documented: a command absent from the
// help is a command nobody finds, and one absent from relayCommands fails deep
// inside a dial instead of asking where the relay is (issue #11).
func TestReadAndEditAreDocumentedAndGated(t *testing.T) {
	// The index lists commands one per line rather than spelling out
	// "wanctl read ..." with its flags; the flags live in the catalog entry
	// that `wanctl help read` renders.
	for _, line := range []string{"  read ", "  edit "} {
		if !strings.Contains(usage, line) {
			t.Errorf("the command index does not list %q", line)
		}
		name := strings.TrimSpace(line)
		if _, ok := catalog.Lookup(name); !ok {
			t.Errorf("%q has no catalog entry, so `wanctl help %s` says nothing", name, name)
		}
	}
	for _, cmd := range []string{"read", "edit"} {
		if !relayCommands[cmd] {
			t.Errorf("%q is missing from relayCommands", cmd)
		}
	}
}

// `wanctl read /path --limit 20` is the order a person types. Go's flag package
// stops at the first non-flag argument, so without this the limit would be
// silently ignored and the caller would get 2000 lines believing they asked for
// 20. The hard case is a flag whose VALUE looks positional.
func TestParseAroundPositionals(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		want   string
		target string
		limit  int
	}{
		{name: "flags first", args: []string{"--target", "ns/dev", "--limit", "20", "/etc/hosts"}, want: "/etc/hosts", target: "ns/dev", limit: 20},
		{name: "flags last", args: []string{"/etc/hosts", "--target", "ns/dev", "--limit", "20"}, want: "/etc/hosts", target: "ns/dev", limit: 20},
		{name: "flags either side", args: []string{"--target", "ns/dev", "/etc/hosts", "--limit", "20"}, want: "/etc/hosts", target: "ns/dev", limit: 20},
		{name: "no flags", args: []string{"/etc/hosts"}, want: "/etc/hosts"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := flag.NewFlagSet("read", flag.ContinueOnError)
			target := fs.String("target", "", "")
			limit := fs.Int("limit", 0, "")
			got := parseAroundPositionals(fs, tt.args)
			if len(got) != 1 || got[0] != tt.want {
				t.Fatalf("positionals = %q, want [%q]", got, tt.want)
			}
			if *target != tt.target || *limit != tt.limit {
				t.Fatalf("target=%q limit=%d, want %q/%d", *target, *limit, tt.target, tt.limit)
			}
		})
	}

	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	fs.String("target", "", "")
	if got := parseAroundPositionals(fs, []string{"/a", "/b"}); len(got) != 2 {
		t.Fatalf("two positionals collapsed to %q; the caller must be able to reject them", got)
	}
}
