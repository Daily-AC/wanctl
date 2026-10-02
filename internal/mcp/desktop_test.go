package mcp

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/jpeg"
	"strings"
	"testing"

	mcpapi "github.com/mark3labs/mcp-go/mcp"
	"wanctl/internal/client"
	"wanctl/internal/protocol"
)

func TestDesktopMCPPassesDeviceJPEGUnchanged(t *testing.T) {
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 1280, 720)), nil); err != nil {
		t.Fatal(err)
	}
	res := desktopToolResponse(nil, &client.DesktopResponse{Result: protocol.DesktopResult{Status: "completed", Snapshot: &protocol.DesktopSnapshot{ID: "s", Width: 1280, Height: 720}, MIME: "image/jpeg", ImageBytes: encoded.Len()}, Image: encoded.Bytes()}, nil)
	if res.IsError || len(res.Content) != 2 {
		t.Fatalf("%+v", res)
	}
	img, ok := res.Content[1].(mcpapi.ImageContent)
	if !ok {
		t.Fatalf("%T", res.Content[1])
	}
	got, err := base64.StdEncoding.DecodeString(img.Data)
	if err != nil || img.MIMEType != "image/jpeg" || !bytes.Equal(got, encoded.Bytes()) {
		t.Fatal("MCP altered coordinate image")
	}
	if !strings.Contains(res.Content[0].(mcpapi.TextContent).Text, `"id":"s"`) {
		t.Fatal("coordinate ID lost")
	}
}
func TestDesktopMCPStopAndUnknownAreErrorsWithPartialEvidence(t *testing.T) {
	res := desktopToolResponse(nil, &client.DesktopResponse{Result: protocol.DesktopResult{Status: "interrupted", Error: protocol.DesktopHumanInput, Completed: 2, FailedIndex: 2}}, nil)
	if !res.IsError || len(res.Content) != 2 || !strings.Contains(res.Content[0].(mcpapi.TextContent).Text, `"completed":2`) || !strings.Contains(res.Content[1].(mcpapi.TextContent).Text, "never retry automatically") {
		t.Fatalf("%+v", res)
	}
	res = desktopToolResponse(nil, nil, &client.DesktopStateUnknownError{})
	if !res.IsError || !strings.Contains(res.Content[0].(mcpapi.TextContent).Text, "do not replay") {
		t.Fatalf("%+v", res)
	}
	secret := "SYNTHETIC_PRIVATE"
	_, err := desktopActions([]any{map[string]any{"type": "click", "x": secret}})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("malformed content leaked: %v", err)
	}
}
