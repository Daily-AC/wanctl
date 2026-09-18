package webfetch

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wanctl/internal/catalog"
)

// Discovery used to describe the primitives in its own words, which is how the
// hand-written copy and the catalog the CLI and MCP server render from came
// apart (issue #100). The rules a delegated caller must arrive with are now the
// catalog's; this test names the ones that were written down after an agent got
// them wrong in the field, so losing one fails CI rather than a user's task.
func TestDiscoveryRendersTheCatalogInstructions(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(getDiscovery(t, staticHandler(t), "?format=json").Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	instructions, ok := doc["instructions"].(string)
	if !ok || instructions != catalog.DelegatedInstructions() {
		t.Fatalf("instructions do not come from the catalog: %v", doc["instructions"])
	}
	for _, want := range []string{
		catalog.Headline,
		// The four primitives this protocol has, under its own names.
		"exec", "read_text", "edit_text", "write_text",
		// Look at a file with the file tools, not with a shell.
		"Never cat/sed/echo a file through a shell",
		// A project's own instructions outrank the agent's plan.
		"AGENTS.md", "CLAUDE.md", "and follow it",
	} {
		if !strings.Contains(instructions, want) {
			t.Errorf("discovery instructions no longer carry %q", want)
		}
	}
	// A delegated session has none of these, and offering one costs the caller
	// a turn and the human a failed task.
	for _, absent := range []string{"wanctl_", "exec_async", "exec_poll", "push", "pull", "screenshot"} {
		if strings.Contains(instructions, absent) {
			t.Errorf("discovery instructions offer %q to a delegated client", absent)
		}
	}
	// WebFetch's own protocol text stays: the catalog knows nothing about
	// nonces, approval or the two moments a human acts.
	for _, key := range []string{"procedure", "human_checkpoints", "security"} {
		if doc[key] == nil {
			t.Errorf("discovery lost its %s", key)
		}
	}
}

func TestDiscoveryHTMLShowsTheInstructions(t *testing.T) {
	body := getDiscovery(t, staticHandler(t), "").Body.String()
	// The HTML page is what a URL reader that mangles JSON actually gets.
	for _, want := range []string{"read_text", "Never cat/sed/echo a file through a shell", "AGENTS.md"} {
		if !strings.Contains(body, want) {
			t.Errorf("the HTML discovery page never shows %q", want)
		}
	}
	w := httptest.NewRecorder()
	staticHandler(t).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/webfetch/v1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("discovery = %d", w.Code)
	}
}
