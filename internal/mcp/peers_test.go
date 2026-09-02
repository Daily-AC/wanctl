package mcp

import (
	"strings"
	"testing"
)

func TestPeerToolResultIncludesAliasesWithoutRemovingDevices(t *testing.T) {
	devices := []string{"legion", "plain"}
	aliases := map[string]string{"legion": "desk"}
	result := peerToolResult(devices, aliases)
	text := resultText(result)
	if !strings.Contains(text, "legion  (desk)") || !strings.Contains(text, "plain") {
		t.Fatalf("text result = %q", text)
	}
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content type = %T", result.StructuredContent)
	}
	gotDevices, ok := structured["devices"].([]string)
	if !ok || len(gotDevices) != 2 || gotDevices[0] != "legion" {
		t.Fatalf("structured devices = %#v", structured["devices"])
	}
	gotAliases, ok := structured["aliases"].(map[string]string)
	if !ok || gotAliases["legion"] != "desk" {
		t.Fatalf("structured aliases = %#v", structured["aliases"])
	}
}
