package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"os"

	"wanctl/internal/mcp"
)

// cmdMCP is the CLI entry for the MCP server. Default mode is stdio (an AI
// host spawns `wanctl mcp` as a child); with --http :addr it serves the same
// 11 tools over a public HTTP/Streamable endpoint that any number of remote
// MCP clients can register a URL against — no per-client binary install
// needed. The HTTP mode requires WANCTL_MCP_SEED env (hex, ≥32 bytes) used as
// the master secret for HKDF-deriving each user's controller identity.
//
// For the team's hosted deployment, the relay's own HTTP mux already mounts
// the same handler at /mcp (see internal/relay/mux wire-up), so users just
// register https://***REMOVED-IP***/mcp with their AI host.
func cmdMCP(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	httpAddr := fs.String("http", "", "if set, serve HTTP/Streamable MCP on this addr (e.g. :8080) instead of stdio")
	fs.Parse(args)

	if *httpAddr == "" {
		return mcp.ServeStdio()
	}
	seedHex := os.Getenv("WANCTL_MCP_SEED")
	if seedHex == "" {
		return fmt.Errorf("WANCTL_MCP_SEED env (hex, ≥32 bytes) is required for --http mode")
	}
	seed, err := hex.DecodeString(seedHex)
	if err != nil {
		return fmt.Errorf("WANCTL_MCP_SEED must be hex-encoded: %w", err)
	}
	return mcp.ServeHTTP(*httpAddr, seed)
}
