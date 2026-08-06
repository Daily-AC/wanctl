package mcp

import (
	"encoding/base64"
	"strings"
	"testing"

	mcpapi "github.com/mark3labs/mcp-go/mcp"
)

func execReq(args map[string]any) mcpapi.CallToolRequest {
	return mcpapi.CallToolRequest{Params: mcpapi.CallToolParams{Arguments: args}}
}

func TestExecSourceRejectsAmbiguousInput(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"neither", map[string]any{}, "either 'command'"},
		{"both", map[string]any{"command": "ls", "script": "ls"}, "not both"},
		{"script without interp", map[string]any{"script": "ls"}, "requires 'interp'"},
		{"unknown interp", map[string]any{"script": "ls", "interp": "ruby"}, "unknown interpreter"},
	}
	for _, c := range cases {
		got, res := execSource(execReq(c.args))
		if res == nil {
			t.Errorf("%s: want an error result, got command %q", c.name, got)
			continue
		}
		if !strings.Contains(resultText(res), c.want) {
			t.Errorf("%s: error should mention %q, got %q", c.name, c.want, resultText(res))
		}
	}
}

func TestExecSourcePassesCommandThrough(t *testing.T) {
	got, res := execSource(execReq(map[string]any{"command": "Get-Date"}))
	if res != nil {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if got != "Get-Date" {
		t.Errorf("got %q", got)
	}
}

// A script's own syntax must never appear in the command handed to the device's
// shell — that is the entire reason the parameter exists.
func TestExecSourceEncodesScript(t *testing.T) {
	src := "Get-ChildItem | Where-Object { $_.Length -gt 0 } | ForEach-Object { \"文件: $($_.Name)\" }"
	got, res := execSource(execReq(map[string]any{"script": src, "interp": "powershell"}))
	if res != nil {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if strings.Contains(got, "$_") || strings.Contains(got, "文件") {
		t.Fatalf("script leaked into the shell command: %s", got)
	}
	if !strings.Contains(got, "-EncodedCommand") {
		t.Errorf("expected an -EncodedCommand invocation, got %q", got)
	}
	i, j := strings.Index(got, "'"), strings.LastIndex(got, "'")
	if _, err := base64.StdEncoding.DecodeString(got[i+1 : j]); err != nil {
		t.Errorf("payload is not valid base64: %v", err)
	}
}

func resultText(res *mcpapi.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(mcpapi.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
