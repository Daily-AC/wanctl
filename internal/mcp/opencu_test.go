package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	mcpapi "github.com/mark3labs/mcp-go/mcp"
	"wanctl/internal/client"
	"wanctl/internal/opencu"
	"wanctl/internal/opencu/ocutest"
	"wanctl/internal/protocol"
)

func exactJSON(t *testing.T, b []byte) any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}
func TestOpenCUMCPPassthroughRealPeer(t *testing.T) {
	c, err := opencu.Start(context.Background(), exec.Command(ocutest.Build(t)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	raw, err := c.Call(context.Background(), "peer_apply", json.RawMessage(`{"large":true,"error":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req := mcpapi.CallToolRequest{}
	req.Params.Arguments = map[string]any{"tool": "peer_apply"}
	handler := escapeDeviceOutput("wanctl_act", func(context.Context, mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
		return desktopToolResponse(nil, &client.DesktopResponse{Result: protocol.DesktopResult{Status: "completed", OpenCU: &protocol.OpenCUResult{Result: raw}}}, nil), nil
	})
	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(exactJSON(t, raw), exactJSON(t, encoded)) {
		t.Fatal("OpenCU content, PNG, isError, _meta or numeric precision changed")
	}
	discovery, err := c.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	d := openCUToolResponse(protocol.DesktopResult{OpenCU: &protocol.OpenCUResult{Discovery: discovery}})
	if d.Content[0].(mcpapi.TextContent).Text != string(discovery) {
		t.Fatal("instructions or tools rewritten")
	}
	raw, err = c.Call(context.Background(), "peer_apply", json.RawMessage(`{"code":"PERSON_ACTIVE"}`))
	if err != nil {
		t.Fatal(err)
	}
	r := openCUToolResponse(protocol.DesktopResult{OpenCU: &protocol.OpenCUResult{Result: raw}})
	if !r.IsError || len(r.Content) != 3 || !strings.Contains(r.Content[2].(mcpapi.TextContent).Text, "never retry automatically") {
		t.Fatal("missing human activity note")
	}
}

func TestOpenCUArgumentsSurviveSDKDecoding(t *testing.T) {
	s := newMCPServer(false)
	var got []byte
	s.AddTool(mcpapi.NewTool("wanctl_act"), func(_ context.Context, r mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
		got, _ = json.Marshal(r.GetArguments()["arguments"])
		return mcpapi.NewToolResultText("ok"), nil
	})
	raw := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wanctl_act","arguments":{"tool":"arbitrary","target":"lab/device","arguments":{"large":9007199254740993,"nested":{"x":0.12345678901234567890123},"text":"unchanged"}}}}`)
	result := s.HandleMessage(context.Background(), raw)
	if !strings.Contains(string(got), "9007199254740993") || !strings.Contains(string(got), "0.12345678901234567890123") {
		t.Fatalf("precision lost: %s result=%+v", got, result)
	}
}
