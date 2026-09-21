package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"wanctl/internal/client"
	"wanctl/internal/protocol"

	mcpapi "github.com/mark3labs/mcp-go/mcp"
)

// References are explicit: hosted MCP clients can create one transport session
// per call, or reuse one across conversations. Neither OAuth nor transport
// session IDs identify a conversation. A harness can inject this field for its
// conversation without sharing a mutable default device on the MCP server.
func workspaceRoute(req mcpapi.CallToolRequest) (string, string, *mcpapi.CallToolResult) {
	target, raw := reqStr(req, "target", ""), reqStr(req, "workspace", "")
	if raw == "" {
		if target == "" {
			return "", "", mcpapi.NewToolResultError("pass target or workspace; there is no account-wide default workspace")
		}
		return target, "", nil
	}
	if target != "" {
		return "", "", mcpapi.NewToolResultError("pass workspace OR target, not both")
	}
	ref, err := client.ParseWorkspace(raw)
	if err != nil {
		return "", "", mcpapi.NewToolResultError(err.Error())
	}
	return ref.Target, ref.ID, nil
}

func workspaceResult(ref client.WorkspaceRef, r *protocol.WorkspaceResult) *mcpapi.CallToolResult {
	out := struct {
		Workspace string `json:"workspace"`
		*protocol.WorkspaceResult
	}{ref.String(), r}
	b, err := json.Marshal(out)
	if err != nil {
		return mcpapi.NewToolResultError(err.Error())
	}
	result := mcpapi.NewToolResultText(string(b))
	result.StructuredContent = out
	if r.Done && (r.Error != "" || r.Code != 0) {
		result.IsError = true
	}
	return result
}

func mcpWorkspace(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	sess := sessions.get(ctx)
	c, hint := workspaceClient(ctx, sess)
	if hint != nil {
		return hint, nil
	}
	action := reqStr(req, "action", "status")
	var ref client.WorkspaceRef
	var err error
	raw := reqStr(req, "workspace", "")
	if action == "enter" && raw == "" {
		if reqStr(req, "root", "") == "" {
			return mcpapi.NewToolResultError("root is required when entering a workspace"), nil
		}
		ref, err = c.PrepareWorkspace(ctx, reqStr(req, "target", ""))
	} else {
		if reqStr(req, "target", "") != "" {
			return mcpapi.NewToolResultError("pass workspace OR target, not both"), nil
		}
		ref, err = client.ParseWorkspace(raw)
	}
	if err != nil {
		return dialErrorResult(sess, err), nil
	}
	switch action {
	case "enter":
		action = "open"
	case "exit":
		action = "close"
	case "attach":
		action = "status"
	case "status", "cancel":
	default:
		return mcpapi.NewToolResultError("action must be enter, attach, status, cancel, or exit"), nil
	}
	r, err := c.Workspace(ctx, ref, action, protocol.Message{Path: reqStr(req, "root", ""), RequestID: reqStr(req, "request_id", "")})
	if err != nil {
		return mcpapi.NewToolResultError(errorTextOf(dialErrorResult(sess, err)) + "\nworkspace=" + ref.String()), nil
	}
	return workspaceResult(ref, r), nil
}

func mcpWorkspaceExec(ctx context.Context, req mcpapi.CallToolRequest, immediate bool) (*mcpapi.CallToolResult, error) {
	target, id, hint := workspaceRoute(req)
	if hint != nil {
		return hint, nil
	}
	command, hint := execSource(req)
	if hint != nil {
		return hint, nil
	}
	sess := sessions.get(ctx)
	c, hint := workspaceClient(ctx, sess)
	if hint != nil {
		return hint, nil
	}
	ref := client.WorkspaceRef{Target: target, ID: id}
	rid := reqStr(req, "request_id", "")
	if rid == "" {
		rid = client.NewRequestID()
	}
	action := "exec"
	source, interp := reqStr(req, "script", ""), reqStr(req, "interp", "")
	if source != "" {
		action = "exec_script"
	} else {
		interp = ""
	}
	wait := 250
	if immediate {
		wait = 0
	}
	r, err := c.Workspace(ctx, ref, action, protocol.Message{
		Command: command, RequestID: rid, Cwd: reqStr(req, "cwd", ""),
		Script: source, Interp: interp, WaitMillis: wait,
		OneShot: reqBool(req, "oneshot"), Elevate: reqBool(req, "elevate"), Via: reqStr(req, "via", ""),
	})
	if err != nil {
		return mcpapi.NewToolResultError(errorTextOf(dialErrorResult(sess, err)) + fmt.Sprintf("\nworkspace=%s request_id=%s", ref.String(), rid)), nil
	}
	return workspaceResult(ref, r), nil
}

func mcpWorkspacePoll(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	target, id, hint := workspaceRoute(req)
	if hint != nil {
		return hint, nil
	}
	rid := reqStr(req, "job_id", "")
	if rid == "" {
		return mcpapi.NewToolResultError("job_id is the request_id returned by workspace exec"), nil
	}
	sess := sessions.get(ctx)
	c, hint := workspaceClient(ctx, sess)
	if hint != nil {
		return hint, nil
	}
	ref := client.WorkspaceRef{Target: target, ID: id}
	r, err := c.Workspace(ctx, ref, "poll", protocol.Message{RequestID: rid, Offset: int64(reqInt(req, "offset"))})
	if err != nil {
		return dialErrorResult(sess, err), nil
	}
	return workspaceResult(ref, r), nil
}
