package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpapi "github.com/mark3labs/mcp-go/mcp"
	"wanctl/internal/client"
	"wanctl/internal/protocol"
)

func TestOpenCUToolTextIsSanitized(t *testing.T) {
	const deviceText = "screen text\x1b]52;c;cHJvb2Y=\a\x1b[2J"
	raw, err := json.Marshal(map[string]any{"content": []any{map[string]string{"type": "text", "text": deviceText}}})
	if err != nil {
		t.Fatal(err)
	}
	req := mcpapi.CallToolRequest{}
	req.Params.Arguments = map[string]any{"tool": "observe"}
	h := escapeDeviceOutput("wanctl_act", func(context.Context, mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
		return desktopToolResponse(nil, &client.DesktopResponse{Result: protocol.DesktopResult{
			Status: "completed", OpenCU: &protocol.OpenCUResult{Result: raw},
		}}, nil), nil
	})
	res, err := h(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var delivered struct {
		Content []struct{ Text string } `json:"content"`
	}
	if err := json.Unmarshal(encoded, &delivered); err != nil || len(delivered.Content) != 1 {
		t.Fatalf("invalid result: %s (%v)", encoded, err)
	}
	if got := delivered.Content[0].Text; got != escapeDeviceText(deviceText) {
		t.Fatalf("OpenCU text bypassed the registered tool sanitizer: %q; want %q", got, escapeDeviceText(deviceText))
	}
}
