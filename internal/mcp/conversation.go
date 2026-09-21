package mcp

import (
	"context"
	"encoding/json"
	"sync"

	mcpapi "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"wanctl/internal/client"
)

// Opt-in and process-scoped: the host must create one stdio MCP process for
// one conversation. Shared HTTP servers must keep explicit references.
type workspaceConversation struct {
	mu       sync.Mutex
	ref      string
	changing bool
	link     *client.WorkspaceLink
}

func newWorkspaceConversation() *workspaceConversation {
	return &workspaceConversation{link: client.NewWorkspaceLink()}
}

type workspaceLinkKey struct{}

func workspaceClient(ctx context.Context, sess sessionAPI) (*client.Client, *mcpapi.CallToolResult) {
	c, hint := sess.client()
	if hint == nil {
		if link, ok := ctx.Value(workspaceLinkKey{}).(*client.WorkspaceLink); ok {
			c.UseWorkspaceLink(link)
		}
	}
	return c, hint
}

func workspaceDataTool(name string) bool {
	switch name {
	case "wanctl_exec", "wanctl_exec_async", "wanctl_exec_poll", "wanctl_read", "wanctl_edit", "wanctl_write":
		return true
	}
	return false
}

func workspaceSessionTool(name string) bool {
	if workspaceDataTool(name) {
		return true
	}
	switch name {
	case "wanctl_workspace", "wanctl_login", "wanctl_logout", "wanctl_status", "wanctl_peers", "wanctl_pair", "wanctl_id", "wanctl_trust", "wanctl_trust_server":
		return true
	}
	return false
}

func (b *workspaceConversation) wrap(name string, next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
		if !workspaceDataTool(name) && name != "wanctl_workspace" {
			return next(ctx, req)
		}
		args := map[string]any{}
		for k, v := range req.GetArguments() {
			args[k] = v
		}
		req.Params.Arguments = args
		b.mu.Lock()
		ref := b.ref
		action := reqStr(req, "action", "status")
		change := name == "wanctl_workspace" && (action == "enter" || action == "attach" || action == "exit")
		fail := func(text string) (*mcpapi.CallToolResult, error) {
			b.mu.Unlock()
			return mcpapi.NewToolResultError(text), nil
		}
		if b.changing {
			return fail("workspace transition in progress; wait for enter/attach/exit to finish")
		}
		if name == "wanctl_workspace" && (action == "enter" || action == "attach") {
			if ref != "" {
				return fail("this conversation already has a workspace; explicitly exit before entering another")
			}
		} else {
			given := reqStr(req, "workspace", "")
			if ref == "" && workspaceDataTool(name) {
				return fail("this conversation has no workspace; call wanctl_workspace(action=enter) or attach first")
			}
			if ref != "" {
				if reqStr(req, "target", "") != "" || (given != "" && given != ref) {
					return fail("this conversation is bound to another route; omit target/workspace or explicitly exit first")
				}
				args["workspace"] = ref
			}
		}
		if change {
			b.changing = true
		}
		b.mu.Unlock()
		if change {
			defer func() { b.mu.Lock(); b.changing = false; b.mu.Unlock() }()
		}
		ctx = context.WithValue(ctx, workspaceLinkKey{}, b.link)
		result, err := next(ctx, req)
		if change && err == nil && result != nil && !result.IsError {
			var state struct {
				Workspace string `json:"workspace"`
				State     string `json:"state"`
			}
			data, encodeErr := json.Marshal(result.StructuredContent)
			if encodeErr == nil && json.Unmarshal(data, &state) == nil {
				b.mu.Lock()
				if action == "exit" && state.State == "closed" {
					b.ref = ""
				}
				if (action == "enter" || action == "attach") && state.Workspace != "" {
					b.ref = state.Workspace
				}
				b.mu.Unlock()
			}
		}
		return result, err
	}
}
