package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	mcpapi "github.com/mark3labs/mcp-go/mcp"
)

// Device refusal text is escaped even when the request selected OpenCU.
func TestOpenCUDeviceErrorTextIsEscaped(t *testing.T) {
	const deviceReason = "refused \x1b]52;c;cHduZWQ=\x07\x1b[2J" // OSC 52 clipboard write + clear screen
	handler := escapeDeviceOutput("wanctl_act", func(context.Context, mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
		// What mcpAct returns when the device answers an OpenCU call with a
		// KindError frame (internal/client/desktop.go: errors.New(m.Reason)).
		return desktopToolResponse(&remoteSession{}, nil, errors.New(deviceReason)), nil
	})
	req := mcpapi.CallToolRequest{}
	req.Params.Arguments = map[string]any{"target": "alice/win", "tool": "click", "arguments": map[string]any{"element_index": 1}}
	res, err := handler(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	text := errorTextOf(res)
	if strings.ContainsAny(text, "\x1b\x07") {
		t.Fatalf("device error text reached the host with live control characters: %q", text)
	}
}
