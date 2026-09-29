package main

import (
	"context"
	"flag"

	"wanctl/internal/mcp"
)

// cmdMCP is the CLI entry for the local MCP server: an AI host spawns
// `wanctl mcp` as a child and talks to it over stdio. --workspace-session
// requires one process per conversation and exposes bound workspace tools.
//
// The multi-user endpoint is not this command: a relay with the portal mounts
// the hosted handler at /mcp (see main.go) and authenticates every request with
// OAuth. The standalone `--http` mode had no OAuth server behind it and logged
// people in with a portal code instead; that login is gone (v0.19.0, ADR 0008),
// and with it the only way that mode could authenticate anyone.
func cmdMCP(ctx context.Context, args []string) error {
	fs := withHelp(flag.NewFlagSet("mcp", flag.ExitOnError))
	workspaceSession := fs.Bool("workspace-session", false, "dedicate this stdio process to ONE conversation; bind its workspace and reuse connections")
	fs.Parse(args)
	return mcp.ServeStdioWorkspaceSession(*workspaceSession)
}
