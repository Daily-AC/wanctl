package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"

	mcpapi "github.com/mark3labs/mcp-go/mcp"
)

func TestEscapeDeviceText(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain text", "plain text"},
		{"中文 ü ✓", "中文 ü ✓"},
		{"new\nlines\rand\ttabs", "new\nlines\rand\ttabs"},
		{"\x1b[2J\x1b]0;title\x07", `\x1b[2J\x1b]0;title\x07`},
		{"nul\x00 del\x7f", `nul\x00 del\x7f`},
		{"c1 \u009b31m \u0085", `c1 \u009b31m \u0085`},
		{"not utf-8 \xff\xfe", `not utf-8 \xff\xfe`},
		{"a real � stays", "a real � stays"},
		{`already \x1b visible`, `already \x1b visible`},
	} {
		if got := escapeDeviceText(tc.in); got != tc.want {
			t.Errorf("escapeDeviceText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Escaped text has nothing left to escape, so text that passes through twice —
// or was already escaped by the device — comes out the same.
func TestEscapeDeviceTextIsIdempotent(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for range 5000 {
		b := make([]byte, rng.IntN(48))
		for i := range b {
			b[i] = byte(rng.IntN(256))
		}
		once := escapeDeviceText(string(b))
		if twice := escapeDeviceText(once); twice != once {
			t.Fatalf("escaping %q twice gave %q, once %q", b, twice, once)
		}
		if hasControl(once) || !utf8.ValidString(once) {
			t.Fatalf("escaping %q left %q", b, once)
		}
	}
}

// Every place a device's words can be copied into a result ends up as a string
// in the text or somewhere in the structured content, whatever Go type holds
// it; all of them come out escaped, and numbers come out exact. wanctl_read's
// content is left alone, but not its errors.
func TestEveryStringInAToolResultIsEscaped(t *testing.T) {
	const evil, shown = "x\x1b[2Jy", `x\x1b[2Jy`
	type reason struct {
		Reason string `json:"reason"`
	}
	build := func(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
		r := mcpapi.NewToolResultText("PAIRING REQUIRED. Reason: " + evil)
		r.StructuredContent = map[string]any{
			"name": evil, "list": []string{evil}, "by_name": map[string]string{evil: evil},
			"nested": reason{evil}, "size": int64(1 << 60),
		}
		return r, nil
	}
	res, err := escapeDeviceOutput("wanctl_peers", build)(context.Background(), mcpapi.CallToolRequest{})
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(res)
	var decoded any
	json.Unmarshal(wire, &decoded)
	flat, _ := json.Marshal(decoded)
	if strings.Contains(string(wire), `\u001b`) || strings.Count(string(flat), `x\\x1b[2Jy`) != 6 {
		t.Fatalf("result on the wire: %s", wire)
	}
	if !strings.Contains(string(wire), `"size":1152921504606846976`) {
		t.Fatalf("a large number did not survive: %s", wire)
	}

	raw, _ := escapeDeviceOutput("wanctl_read", build)(context.Background(), mcpapi.CallToolRequest{})
	if text := raw.Content[0].(mcpapi.TextContent).Text; !strings.Contains(text, evil) {
		t.Fatalf("wanctl_read content was escaped: %q", text)
	}
	failed, _ := escapeDeviceOutput("wanctl_read", func(context.Context, mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
		return mcpapi.NewToolResultError("device said: " + evil), nil
	})(context.Background(), mcpapi.CallToolRequest{})
	if text := failed.Content[0].(mcpapi.TextContent).Text; text != "device said: "+shown {
		t.Fatalf("wanctl_read error = %q, want it escaped", text)
	}
	_, err = escapeDeviceOutput("wanctl_exec", func(context.Context, mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
		return nil, errors.New("device said: " + evil)
	})(context.Background(), mcpapi.CallToolRequest{})
	if err == nil || err.Error() != "device said: "+shown {
		t.Fatalf("handler error = %v, want it escaped", err)
	}
}
