package mcp

import (
	"context"
	"testing"

	"wanctl/internal/transport"

	mcpapi "github.com/mark3labs/mcp-go/mcp"
)

func TestMCPTrustServerPinsExactTarget(t *testing.T) {
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
