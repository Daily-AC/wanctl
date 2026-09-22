package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"wanctl/internal/client"
	"wanctl/internal/protocol"
	"wanctl/internal/script"
)

// The binding belongs to the caller's environment, never the account's config
// directory. Separate harnesses can inherit different refs under the same login.
func workspaceFlag(fs *flag.FlagSet) *string {
	return fs.String("workspace", os.Getenv("WANCTL_WORKSPACE"), "workspace reference (defaults to WANCTL_WORKSPACE in this caller's environment)")
}

func workspaceRoute(target, raw string) (client.WorkspaceRef, error) {
	if raw == "" {
		return client.WorkspaceRef{Target: target}, nil
	}
	if target != "" {
		return client.WorkspaceRef{}, fmt.Errorf("pass workspace OR target, not both; unset WANCTL_WORKSPACE before using a legacy target")
	}
	return client.ParseWorkspace(raw)
}

func printWorkspace(ref client.WorkspaceRef, result *protocol.WorkspaceResult) error {
	return json.NewEncoder(os.Stdout).Encode(struct {
		Workspace string `json:"workspace"`
		*protocol.WorkspaceResult
	}{ref.String(), result})
}

func cmdWorkspace(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: wanctl workspace enter|attach|status|poll|cancel|exit [options]; see wanctl help workspace")
	}
	action := args[0]
	switch action {
	case "enter", "attach", "status", "poll", "cancel", "exit":
	default:
		return fmt.Errorf("unknown workspace action %q", action)
	}
	fs := withHelp(flag.NewFlagSet("workspace", flag.ContinueOnError))
	raw := workspaceFlag(fs)
	target := fs.String("target", "", "device to enter")
	root := fs.String("root", "", "absolute project root on the device")
	rid := fs.String("request-id", "", "command to inspect, poll or cancel")
	offset := fs.Int64("offset", 0, "byte offset returned by the previous poll")
	if err := fs.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected workspace argument %q", fs.Arg(0))
	}
	if *offset < 0 || (*offset != 0 && action != "poll") {
		return fmt.Errorf("--offset must be nonnegative and is only supported by poll")
	}
	if (action == "poll" || action == "cancel") && *rid == "" {
		return fmt.Errorf("--request-id is required for %s", action)
	}
	if *rid != "" && action != "poll" && action != "cancel" && action != "status" {
		return fmt.Errorf("--request-id is only supported by status, poll and cancel")
	}
	if action == "enter" && *root == "" {
		return fmt.Errorf("--root is required for enter")
	}
	if action != "enter" && (*target != "" || *root != "") {
		return fmt.Errorf("--target and --root are only supported by enter")
	}
	ref, err := workspaceRoute(*target, *raw)
	if err != nil {
		return err
	}
	if action != "enter" && ref.ID == "" {
		return fmt.Errorf("pass --workspace or set WANCTL_WORKSPACE; there is no account-wide current workspace")
	}
	c, err := client.New()
	if err != nil {
		return err
	}
	if action == "enter" && ref.ID == "" {
		ref, err = c.PrepareWorkspace(ctx, *target)
		if err != nil {
			return err
		}
	}
	// Print the chosen ID before opening; even a killed controller can recover
	// an uncertain open by passing the same reference and root to enter.
	if action == "enter" {
		fmt.Fprintf(os.Stderr, "workspace=%s\n", ref.String())
	}
	switch action {
	case "enter":
		action = "open"
	case "attach":
		action = "status"
	case "exit":
		action = "close"
	}
	result, err := c.Workspace(ctx, ref, action, protocol.Message{Path: *root, RequestID: *rid, Offset: *offset})
	if err != nil {
		return err
	}
	return printWorkspace(ref, result)
}

// Return the remote exit code separately from transport errors. Interrupting
// the waiter deliberately does not kill the device's shell or its command.
func execWorkspace(ctx context.Context, c *client.Client, ref client.WorkspaceRef, req protocol.Message, sourcePath, interp string, async bool, out, diagnostic io.Writer) (int, error) {
	if req.OneShot || req.Elevate || req.Via != "" {
		return 1, fmt.Errorf("workspace exec does not support --oneshot, --elevate or --via")
	}
	if req.RequestID == "" {
		req.RequestID = client.NewRequestID()
	}
	action := "exec"
	if sourcePath != "" {
		// Read once: policy command and source must describe the same bytes,
		// even if another process is changing the local file while we submit.
		data, err := os.ReadFile(sourcePath)
		if err != nil {
			return 1, err
		}
		var in script.Interp
		if interp != "" {
			in, err = script.ParseInterp(interp)
		} else {
			in, err = script.ForPath(sourcePath)
		}
		if err != nil {
			return 1, err
		}
		req.Command, err = script.Command(in, data)
		if err != nil {
			return 1, err
		}
		req.Script, req.Interp = string(data), string(in)
		action = "exec_script"
	}
	fmt.Fprintf(diagnostic, "workspace=%s request_id=%s\n", ref.String(), req.RequestID)
	if !async {
		req.WaitMillis = 250
	}
	result, err := c.Workspace(ctx, ref, action, req)
	if err != nil {
		return 1, err
	}
	if async {
		return 0, json.NewEncoder(out).Encode(struct {
			Workspace string `json:"workspace"`
			*protocol.WorkspaceResult
		}{ref.String(), result})
	}
	for {
		if _, err := io.WriteString(out, result.Output); err != nil {
			return 1, err
		}
		if result.Done && result.NextOffset >= result.RetainedBytes {
			if result.Truncated {
				fmt.Fprintln(diagnostic, "wanctl: workspace output truncated; redirect large output to a remote file")
			}
			if result.Error != "" {
				return 1, fmt.Errorf("%s", result.Error)
			}
			return result.Code, nil
		}
		// Drain retained pages immediately; wait only when caught up.
		if result.NextOffset >= result.RetainedBytes {
			timer := time.NewTimer(250 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return 1, ctx.Err()
			case <-timer.C:
			}
		}
		result, err = c.Workspace(ctx, ref, "poll", protocol.Message{RequestID: req.RequestID, Offset: result.NextOffset})
		if err != nil {
			return 1, err
		}
	}
}
