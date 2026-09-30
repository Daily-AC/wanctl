package mcp

import (
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/server"
)

// wanctl_read and wanctl_edit are offered on the shared HTTP endpoint as well
// as over stdio, unlike wanctl_push and wanctl_pull. Those two name a path on
// the MCP server's own disk, which on a shared server belongs to somebody else;
// these name a path on the target device, which is the thing the caller was
// authorized to drive. Registration is shared, so the way this can regress is a
// handler that reaches for a local path -- which is what the second half checks.
func TestReadAndEditAreRegisteredForBothTransports(t *testing.T) {
	for _, hosted := range []bool{false, true} {
		s := server.NewMCPServer("wanctl", "1.0.0")
		registerMCPTools(s, hosted)
		tools := s.ListTools()
		for _, name := range []string{"wanctl_read", "wanctl_edit", "wanctl_push", "wanctl_pull"} {
			if _, ok := tools[name]; !ok {
				t.Fatalf("hosted=%v: %s is not registered", hosted, name)
			}
		}
	}
}

// The descriptions are the only documentation a model reads before choosing a
// tool, so the things it has to do with an error text -- relay a pairing URL,
// re-read after a hash mismatch, stop using exec for file work -- have to be in
// them.
func TestFileToolDescriptionsPointAtEachOther(t *testing.T) {
	s := server.NewMCPServer("wanctl", "1.0.0")
	registerMCPTools(s, false)
	tools := s.ListTools()

	want := map[string][]string{
		"wanctl_read":      {"offset", "truncated", "sha256", "not a UTF-8 text file", "PAIRING REQUIRED"},
		"wanctl_edit":      {"expected_sha256", "old string not found", "occurs N times", "atomic"},
		"wanctl_exec":      {"wanctl_read", "wanctl_edit"},
		"wanctl_pull":      {"wanctl_read"},
		"wanctl_push_blob": {"wanctl_edit"},
		"wanctl_push":      {"wanctl_edit"},
	}
	for name, phrases := range want {
		tool, ok := tools[name]
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		for _, phrase := range phrases {
			if !strings.Contains(tool.Tool.Description, phrase) {
				t.Errorf("%s description does not mention %q", name, phrase)
			}
		}
	}
}
