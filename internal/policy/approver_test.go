package policy

import (
	"bytes"
	"strings"
	"testing"
)

func TestConsoleApproverVerbs(t *testing.T) {
	cases := []struct {
		in       string
		allow    bool
		remember bool
		scope    Scope
	}{
		{"y\n", true, false, ""},
		{"a\n", true, true, ScopeDir},
		{"g\n", true, true, ScopeGlobal},
		{"n\n", false, false, ""},
		{"\n", false, false, ""},      // empty defaults to deny
		{"nope\n", false, false, ""},  // anything else denies
	}
	for _, c := range cases {
		ap := NewConsoleApprover(strings.NewReader(c.in), &bytes.Buffer{})
		d := ap.Ask(Request{Kind: KindExec, Cmd: "rm -rf /", Peer: "fp"})
		if d.Allow != c.allow || d.Remember != c.remember || d.Scope != c.scope {
			t.Errorf("input %q => %+v, want allow=%v remember=%v scope=%v", c.in, d, c.allow, c.remember, c.scope)
		}
	}
}
