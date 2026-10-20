package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

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

// workspaceResult renders one workspace answer as the few lines a caller acts
// on. A lifecycle answer names the reference to carry; a command answer leads
// with its request and outcome, then the output. Fields that only repeat what
// the caller sent (the reference, the root) or that hold their zero value are
// left out: every exec in a workspace pays for this header again.
func workspaceResult(ref client.WorkspaceRef, r *protocol.WorkspaceResult) *mcpapi.CallToolResult {
	result := mcpapi.NewToolResultText(workspaceText(ref, r))
	if r.Done && (r.Error != "" || r.Code != 0) {
		result.IsError = true
	}
	return result
}

func workspaceText(ref client.WorkspaceRef, r *protocol.WorkspaceResult) string {
	var b strings.Builder
	if r.RequestID == "" {
		fmt.Fprintf(&b, "workspace: %s\nroot: %s\nstate: %s\n", ref.String(), r.Root, r.State)
		if r.ActiveRequestID != "" {
			fmt.Fprintf(&b, "active request: %s\n", r.ActiveRequestID)
		}
		return b.String()
	}
	switch {
	case r.Done:
		fmt.Fprintf(&b, "request %s: done, exit %d\n", r.RequestID, r.Code)
	case r.RequestState == "approving":
		fmt.Fprintf(&b, "request %s: waiting for the device owner's approval\n", r.RequestID)
	default:
		fmt.Fprintf(&b, "request %s: running\n", r.RequestID)
	}
	if r.Error != "" {
		fmt.Fprintf(&b, "error: %s\n", r.Error)
	}
	if r.State != "open" {
		fmt.Fprintf(&b, "workspace state: %s\n", r.State)
	}
	if r.ActiveRequestID != "" && r.ActiveRequestID != r.RequestID {
		// Inspecting an old request must not hide the one now running: it
		// is the id a caller needs to poll or cancel.
		fmt.Fprintf(&b, "active request: %s\n", r.ActiveRequestID)
	}
	if r.Done && r.Cwd != "" {
		// Shown even when it is the root: leaving it out would read the same
		// as "the shell could not say", after a command that went back.
		fmt.Fprintf(&b, "cwd: %s\n", r.Cwd)
	}
	if !r.Done || r.NextOffset < r.RetainedBytes {
		fmt.Fprintf(&b, "next_offset: %d\n", r.NextOffset)
	}
	if r.Truncated {
		fmt.Fprintf(&b, "the device kept only the first %d of %d output bytes\n", r.RetainedBytes, r.OutputBytes)
	}
	if r.Output == "" {
		if r.Done && r.OutputBytes == 0 {
			b.WriteString("(no output)\n")
		}
		return b.String()
	}
	b.WriteString("\n")
	b.WriteString(r.Output)
	if !strings.HasSuffix(r.Output, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

// workspaceOutcome carries what a lifecycle call left behind to the
// conversation binding, which has to learn the reference a successful enter
// returned without parsing the text written for the model.
type workspaceOutcome struct {
	ref, state string
}

type workspaceOutcomeKey struct{}

func reportWorkspaceOutcome(ctx context.Context, ref client.WorkspaceRef, r *protocol.WorkspaceResult) {
	if sink, ok := ctx.Value(workspaceOutcomeKey{}).(*workspaceOutcome); ok {
		sink.ref, sink.state = ref.String(), r.State
	}
}

// execWait is how long one exec or poll call holds on to a command that has
// not finished. Without it a command that takes two seconds costs the caller a
// second tool call, and a build costs one call per glance; with it most
// commands come back finished in the call that started them, and a long one
// costs a call every twenty seconds. It stays well under the timeouts hosts put
// on a tool call. A variable so tests can shorten it.
var execWait = 20 * time.Second

// pollPause paces the re-checks inside one waiting call: quick at first,
// because most commands finish within a second or two, then every two seconds.
func pollPause(i int) time.Duration {
	switch i {
	case 0:
		return 250 * time.Millisecond
	case 1:
		return 500 * time.Millisecond
	case 2:
		return time.Second
	}
	return 2 * time.Second
}

// sleepCtx waits for d unless ctx ends first, and reports whether it waited.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// waitWorkspace re-polls a request until it is done or execWait runs out. A
// failed re-poll ends the wait with the last answer that did arrive: the
// command keeps running on the device either way, and the caller's next poll
// reports whatever went wrong.
func waitWorkspace(ctx context.Context, c *client.Client, ref client.WorkspaceRef, offset int64, r *protocol.WorkspaceResult) *protocol.WorkspaceResult {
	// ctx carries the whole budget (see settleWorkspace). It bounds the
	// re-polls too, not only the pauses between them: a poll that stalls past
	// it is cut off and the last answer stands. Re-polling stops early enough
	// to leave clampWorkspace its one fetch inside the same budget.
	if deadline, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline.Add(-min(clampReserve, execWait/10)))
		defer cancel()
	}
	for i := 0; !r.Done; i++ {
		if !sleepCtx(ctx, pollPause(i)) {
			break
		}
		next, err := c.Workspace(ctx, ref, "poll", protocol.Message{RequestID: r.RequestID, Offset: offset})
		if err != nil {
			break
		}
		r = next
	}
	return r
}

// clampReserve is the part of execWait kept for clampWorkspace's tail fetch.
const clampReserve = 2 * time.Second

// settleWorkspace waits for a request and bounds what it returns, all within
// one execWait: a host's deadline on the call has to cover both.
func settleWorkspace(ctx context.Context, c *client.Client, ref client.WorkspaceRef, offset int64, r *protocol.WorkspaceResult) *protocol.WorkspaceResult {
	ctx, cancel := context.WithTimeout(ctx, execWait)
	defer cancel()
	return clampWorkspace(ctx, c, ref, offset, waitWorkspace(ctx, c, ref, offset, r))
}

// clampWorkspace bounds what one answer carries the way polled legacy output
// is bounded: everything from offset when it fits in maxExecStream, otherwise
// its first clampHead bytes and its last bytes around a line naming the offset
// the gap starts at. next_offset is the end either way, so a long build costs
// one page per look instead of one page per 48 KiB of output.
//
// The offsets are device bytes, but the output arrives as JSON text, in which
// each byte that is not UTF-8 has become a three-byte U+FFFD. Byte arithmetic
// on the text is only true where the text is exactly as long as the bytes it
// came from, so whenever a page is not, the forward page the device itself cut
// is returned instead: longer, but every offset in it is the device's own.
func clampWorkspace(ctx context.Context, c *client.Client, ref client.WorkspaceRef, offset int64, r *protocol.WorkspaceResult) *protocol.WorkspaceResult {
	if r.RequestID == "" || r.RetainedBytes-offset <= maxExecStream || int64(len(r.Output)) != r.NextOffset-offset {
		return r
	}
	tailN := int64(maxExecStream - clampHead)
	tailStart := r.RetainedBytes - tailN
	t, err := c.Workspace(ctx, ref, "poll", protocol.Message{RequestID: r.RequestID, Offset: tailStart})
	if err != nil {
		// The forward page already in hand is still true, and its
		// next_offset says where to continue.
		return r
	}
	// tailStart is an arbitrary byte, so it can split a character, whose
	// remaining bytes arrive as one U+FFFD each: two text bytes more than the
	// device sent, per byte. Only that excess identifies them; a U+FFFD the
	// command really printed is as long as its bytes and stays.
	tail := t.Output
	excess := int64(len(tail)) - (t.NextOffset - tailStart)
	for i := 0; i < utf8.UTFMax-1 && excess > 0 && strings.HasPrefix(tail, "\uFFFD"); i++ {
		tail, tailStart, excess = tail[len("\uFFFD"):], tailStart+1, excess-2
	}
	if excess != 0 {
		return r
	}
	if int64(len(tail)) > tailN {
		// The command kept writing between the two polls, and the device
		// pages a full 48 KiB from tailStart. Keep the page within budget;
		// next_offset then points at what follows instead of the end.
		cut := int(tailN)
		for cut > 0 && !utf8.RuneStart(tail[cut]) {
			cut--
		}
		t.NextOffset -= int64(len(tail) - cut)
		tail = tail[:cut]
	}
	head := r.Output
	if len(head) > clampHead {
		cut := clampHead
		for cut > 0 && !utf8.RuneStart(head[cut]) {
			cut--
		}
		head = head[:cut]
	}
	gap := offset + int64(len(head))
	out := *t
	out.Output = head + fmt.Sprintf("\n[... %d bytes skipped; poll with offset=%d to read from there ...]\n", tailStart-gap, gap) + tail
	return &out
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
	reportWorkspaceOutcome(ctx, ref, r)
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
	// The device holds the first answer up to a second; waitWorkspace takes
	// it from there. exec_async asks for neither.
	wait := 1000
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
	if !immediate {
		r = settleWorkspace(ctx, c, ref, 0, r)
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
	offset := int64(reqInt(req, "offset"))
	r, err := c.Workspace(ctx, ref, "poll", protocol.Message{RequestID: rid, Offset: offset})
	if err != nil {
		return dialErrorResult(sess, err), nil
	}
	r = settleWorkspace(ctx, c, ref, offset, r)
	return workspaceResult(ref, r), nil
}
