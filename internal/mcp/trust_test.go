package mcp

import (
	"context"
	"strings"
	"testing"

	"wanctl/internal/transport"

	mcpapi "github.com/mark3labs/mcp-go/mcp"
)

func TestMCPTrustServerPinsExactTarget(t *testing.T) {
	t.Setenv("WANCTL_MCP_ALLOW_UNSAFE_TRUST_SERVER", "1")
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_TOKEN", "tok")
	t.Setenv("WANCTL_RELAY", "http://relay.invalid")
	sessions = &sessionStore{stdio: &localFsSession{}}
	fp := transport.Fingerprint([]byte("verified device cert"))
	req := mcpapi.CallToolRequest{Params: mcpapi.CallToolParams{Arguments: map[string]any{
		"target": "alice/build", "fingerprint": fp,
	}}}
	result, err := mcpTrustServer(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("trust tool returned error: %+v", result)
	}
	store, err := transport.OpenStore("known_servers.json")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := store.GetByName("alice/build")
	if !ok || got.Fingerprint != fp {
		t.Fatalf("exact MCP pin missing: %+v, ok=%v", got, ok)
	}
}

func TestMCPTrustServerIsDisabledByDefault(t *testing.T) {
	t.Setenv("WANCTL_MCP_ALLOW_UNSAFE_TRUST_SERVER", "")
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_TOKEN", "tok")
	t.Setenv("WANCTL_RELAY", "http://relay.invalid")
	sessions = &sessionStore{stdio: &localFsSession{}}
	fp := transport.Fingerprint([]byte("relay-supplied device cert"))
	req := mcpapi.CallToolRequest{Params: mcpapi.CallToolParams{Arguments: map[string]any{
		"target": "alice/build", "fingerprint": fp,
	}}}
	result, err := mcpTrustServer(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(toolText(result), "disabled in MCP") {
		t.Fatalf("trust tool result = %+v", result)
	}
	store, err := transport.OpenStore("known_servers.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.GetByName("alice/build"); ok {
		t.Fatal("the default MCP flow pinned a relay-supplied identity")
	}
}
