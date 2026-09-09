package mcp

import (
	"strings"
	"testing"

	"wanctl/internal/client"
)

func TestPeerToolResultIncludesAliasesWithoutRemovingDevices(t *testing.T) {
	devices := []string{"legion", "plain"}
	aliases := map[string]string{"legion": "desk"}
	result := peerToolResult(devices, aliases, nil)
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

// A grantee's only route to a shared device is its owner/device target, so the
// tool has to print that spelling, alongside their own devices and not instead
// of them.
func TestPeerToolResultListsSharedDevicesAsQualifiedTargets(t *testing.T) {
	shared := []client.SharedDevice{
		{Owner: "daily-ac", Device: "8e894048-1111-4222-8333-444455556666", Label: "bms-20558674", Target: "daily-ac/8e894048-1111-4222-8333-444455556666", Perms: "exec,read,write", Online: true},
		{Owner: "daily-ac", Device: "aa11", Label: "spare", Target: "daily-ac/aa11"},
	}
	text := resultText(peerToolResult([]string{"mine"}, nil, shared))
	if !strings.Contains(text, "mine") {
		t.Fatalf("own devices dropped: %q", text)
	}
	if !strings.Contains(text, "daily-ac/8e894048-1111-4222-8333-444455556666  (bms-20558674)") {
		t.Fatalf("shared target missing: %q", text)
	}
	if !strings.Contains(text, "daily-ac/aa11  (spare)  [offline]") {
		t.Fatalf("offline marker missing: %q", text)
	}
}

// A token with no devices of its own but a grant from someone else must not be
// told there is nothing to talk to.
func TestPeerToolResultReportsSharedOnlyTokens(t *testing.T) {
	shared := []client.SharedDevice{{Owner: "daily-ac", Device: "aa11", Label: "bms", Target: "daily-ac/aa11", Online: true}}
	text := resultText(peerToolResult(nil, nil, shared))
	if strings.Contains(text, "no devices online") || !strings.Contains(text, "daily-ac/aa11") {
		t.Fatalf("shared-only result = %q", text)
	}
}
