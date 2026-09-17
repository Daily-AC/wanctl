package mcp

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"

	"wanctl/internal/catalog"

	"github.com/mark3labs/mcp-go/server"
)

// registration is the shape of testdata/mcp_registration.json: the wire
// contract an AI host sees. It was captured from the registration that existed
// before the catalog moved the text out of this file, and it is the thing that
// must not move. Descriptions are deliberately absent — those are meant to
// improve; names, argument names, types and required flags are not.
type registration struct {
	Name   string     `json:"name"`
	Params []regParam `json:"params"`
}

type regParam struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
}

func currentRegistration(t *testing.T) []registration {
	t.Helper()
	s := server.NewMCPServer("wanctl", "1.0.0")
	registerMCPTools(s)
	tools := s.ListTools()

	names := make([]string, 0, len(tools))
	for n := range tools {
		names = append(names, n)
	}
	sort.Strings(names)

	out := make([]registration, 0, len(names))
	for _, n := range names {
		schema := tools[n].Tool.InputSchema
		required := map[string]bool{}
		for _, r := range schema.Required {
			required[r] = true
		}
		props := make([]string, 0, len(schema.Properties))
		for k := range schema.Properties {
			props = append(props, k)
		}
		sort.Strings(props)

		entry := registration{Name: n, Params: []regParam{}}
		for _, p := range props {
			raw, err := json.Marshal(schema.Properties[p])
			if err != nil {
				t.Fatalf("marshal %s.%s: %v", n, p, err)
			}
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("unmarshal %s.%s: %v", n, p, err)
			}
			typ, _ := m["type"].(string)
			entry.Params = append(entry.Params, regParam{Name: p, Type: typ, Required: required[p]})
		}
		out = append(out, entry)
	}
	return out
}

// Moving the tool text into the catalog must not move one byte of the schema.
// A renamed argument or a dropped `required` silently breaks every agent that
// already learned this API, and nothing in a passing build would say so.
func TestRegistrationMatchesSnapshot(t *testing.T) {
	want, err := os.ReadFile("testdata/mcp_registration.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(currentRegistration(t), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != strings.TrimSpace(string(want)) {
		t.Errorf("MCP registration changed.\n--- want (testdata/mcp_registration.json)\n%s\n--- got\n%s",
			want, got)
	}
}

// The catalog is only a single source if nothing registers behind its back.
func TestEveryCatalogToolIsRegistered(t *testing.T) {
	s := server.NewMCPServer("wanctl", "1.0.0")
	registerMCPTools(s)
	tools := s.ListTools()

	for _, c := range catalog.MCPCommands() {
		tool, ok := tools[c.MCPName]
		if !ok {
			t.Errorf("catalog declares %s but it is not registered", c.MCPName)
			continue
		}
		if tool.Tool.Description != c.MCPDescription() {
			t.Errorf("%s description does not come from the catalog", c.MCPName)
		}
	}
	if len(tools) != len(catalog.MCPCommands()) {
		t.Errorf("registered %d tools, catalog declares %d", len(tools), len(catalog.MCPCommands()))
	}
}

// mustKeep is the list of rules that were written into the tool descriptions
// one incident at a time: what to do with a pairing URL, how first contact
// differs from a changed identity, why a script beats a quoted command, that
// elevate needs its own approval, that a persistent session is the default.
// Rewriting the prose is welcome; dropping one of these is a regression an AI
// caller pays for, silently, in the field.
func TestDescriptionsKeepTheRules(t *testing.T) {
	s := server.NewMCPServer("wanctl", "1.0.0")
	registerMCPTools(s)
	tools := s.ListTools()

	mustKeep := map[string][]string{
		"wanctl_exec": {
			"PAIRING REQUIRED",
			// What to do with an output too long to return whole.
			"LONG OUTPUT", "LAST 48 KiB", "grep that file",
			"VERBATIM",
			"DEVICE IDENTITY CONFIRMATION REQUIRED",
			"wanctl_trust_server",
			"wanctl_read",
			"wanctl_edit",
			// The dev loop: a persistent session, background jobs, file tools.
			"wanctl_exec_async",
			"wanctl_exec_poll",
			"wanctl_push_blob",
			"cwd",
			// Read the project's own instructions before acting in it.
			"AGENTS.md",
			"CLAUDE.md",
			"and follow it",
		},
		"wanctl_login": {"LOGIN REQUIRED", "rebind"},
		"wanctl_pair":  {"PAIRING REQUIRED", "DEVICE IDENTITY CONFIRMATION REQUIRED"},
		"wanctl_trust_server": {
			"DEVICE IDENTITY CONFIRMATION REQUIRED",
			"DEVICE IDENTITY MISMATCH",
			"WANCTL_MCP_ALLOW_UNSAFE_TRUST_SERVER",
		},
		"wanctl_read": {
			"offset", "truncated", "sha256", "not a UTF-8 text file",
			"PAIRING REQUIRED", "256 KiB",
			"AGENTS.md", "CLAUDE.md", "and follow it",
		},
		"wanctl_edit": {
			"expected_sha256", "old string not found", "occurs N times",
			"atomic", "changed since it was read",
			// The batch form, and the four rules that make it worth using:
			// one call instead of several, minimal `old`, no padding, and
			// every entry read against the original text.
			"SEVERAL EDITS AT ONCE",
			"ONE call with several entries rather than several calls",
			"as SMALL as it can be",
			"do not pad it with unchanged lines",
			"not the result of the entry before it",
			"checked before anything is written",
		},
		"wanctl_write": {
			// Which of the two writing tools to reach for, in pi's words.
			"only for NEW files or COMPLETE rewrites",
			"wanctl_edit", "wanctl_push_blob",
			"8 MiB", "atomic", "Missing parent directories are created",
		},
		"wanctl_screenshot": {
			// A capture is gated harder than a command, and a caller has to
			// know what it is looking at when the image was shrunk.
			"ELEVATED", "downscaled", "screencapture", "grim",
			"PAIRING REQUIRED",
		},
		"wanctl_exec_async": {"job_id", "wanctl_exec_poll", "30 minutes"},
		"wanctl_exec_poll":  {"next_offset"},
		"wanctl_push":       {"wanctl_edit", "WANCTL_MCP_LOCAL_ROOT"},
		"wanctl_push_blob":  {"wanctl_edit", "8 MiB"},
		"wanctl_pull":       {"wanctl_read"},
	}
	// Rules that live in a parameter description rather than the tool's own.
	mustKeepParam := map[string]map[string][]string{
		"wanctl_edit": {
			"edits": {"ORIGINAL", "exactly once", "may not overlap", "Mutually exclusive"},
		},
		"wanctl_exec": {
			"script":  {"Requires 'interp'", "encoded"},
			"command": {"parsed TWICE", "is not recognized"},
			"oneshot": {"Default false", "share cwd/env"},
			"elevate": {"OWN policy rule", "bypass mode"},
			"via":     {"su", "adb"},
		},
	}

	for name, phrases := range mustKeep {
		tool, ok := tools[name]
		if !ok {
			t.Errorf("%s is not registered", name)
			continue
		}
		for _, phrase := range phrases {
			if !strings.Contains(tool.Tool.Description, phrase) {
				t.Errorf("%s description no longer says %q", name, phrase)
			}
		}
	}
	for name, params := range mustKeepParam {
		tool, ok := tools[name]
		if !ok {
			t.Errorf("%s is not registered", name)
			continue
		}
		for param, phrases := range params {
			raw, err := json.Marshal(tool.Tool.InputSchema.Properties[param])
			if err != nil {
				t.Fatalf("marshal %s.%s: %v", name, param, err)
			}
			for _, phrase := range phrases {
				if !strings.Contains(string(raw), phrase) {
					t.Errorf("%s.%s description no longer says %q", name, param, phrase)
				}
			}
		}
	}
}

// The instructions ride on the initialize response, which is the one moment a
// host reads anything before deciding how to use the tools. Asserted through a
// real round trip rather than a field read: what matters is that a client sees
// it, not that the struct holds it.
func TestInitializeCarriesTheInstructions(t *testing.T) {
	s := newMCPServer()
	raw := s.HandleMessage(context.Background(), []byte(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`))
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(encoded, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Error) > 0 {
		t.Fatalf("initialize failed: %s", resp.Error)
	}
	if resp.Result.Instructions != catalog.Instructions() {
		t.Errorf("initialize instructions are not the catalog's:\n%s", resp.Result.Instructions)
	}
	// The one rule no tool description can carry on its own.
	if !strings.Contains(resp.Result.Instructions, "AGENTS.md") {
		t.Error("the instructions a host receives do not mention AGENTS.md")
	}
}
