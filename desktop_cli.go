package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"wanctl/internal/client"
	"wanctl/internal/desktop"
	"wanctl/internal/opencu"
	"wanctl/internal/protocol"
)

func cmdScreenshot(ctx context.Context, args []string) error { return cmdDesktop(ctx, args, false) }
func cmdAct(ctx context.Context, args []string) error        { return cmdDesktop(ctx, args, true) }
func cmdDesktop(ctx context.Context, args []string, act bool) error {
	ctx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	name := "screenshot"
	if act {
		name = "act"
	}
	fs := withHelp(flag.NewFlagSet(name, flag.ContinueOnError))
	target := fs.String("target", "", "device ID or unique name")
	output := fs.String("o", "", "legacy image file or OpenCU result JSON; - writes the complete result to stdout")
	id := fs.String("screenshot-id", "", "screenshot coordinate reference")
	var via, region, actions, actionsFile, tool, arguments, argumentsFile, workspace string
	var discover bool
	if act {
		fs.BoolVar(&discover, "discover", false, "discover OpenCU version, instructions and tools")
		fs.StringVar(&tool, "tool", "", "OpenCU tool name from discovery")
		fs.StringVar(&arguments, "arguments", "", "OpenCU arguments JSON object")
		fs.StringVar(&argumentsFile, "arguments-file", "", "arguments JSON file (- for stdin)")
		fs.StringVar(&workspace, "workspace", "", "OpenCU workspace reference; defaults to WANCTL_WORKSPACE for OpenCU mode")
		fs.StringVar(&actions, "actions", "", "JSON action array")
		fs.StringVar(&actionsFile, "actions-file", "", "read JSON action array from a file (- for stdin)")
	} else {
		fs.StringVar(&via, "via", "", "Android elevation channel: su | adb")
		fs.StringVar(&region, "region", "", "crop x,y,width,height in full-screenshot pixels")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if *target == "" && len(rest) > 0 {
		*target = rest[0]
		if err := fs.Parse(rest[1:]); err != nil {
			return err
		}
		rest = fs.Args()
	}
	if len(rest) > 0 {
		return errors.New("unexpected argument; see wanctl help " + name)
	}
	req := protocol.DesktopRequest{ScreenshotID: *id, Discover: discover, Tool: tool}
	var ref client.WorkspaceRef
	openCU := discover || tool != "" || arguments != "" || argumentsFile != "" || workspace != ""
	if openCU {
		if actions != "" || actionsFile != "" {
			return errors.New("OpenCU mode is mutually exclusive with actions")
		}
		if workspace == "" {
			workspace = os.Getenv("WANCTL_WORKSPACE")
		}
		var err error
		if *target != "" {
			return errors.New("pass workspace OR target, not both")
		}
		ref, err = client.ParseWorkspace(workspace)
		if err != nil {
			return err
		}
		req.WorkspaceID = ref.ID
		if arguments != "" && argumentsFile != "" {
			return errors.New("use either --arguments or --arguments-file")
		}
		req.Arguments = json.RawMessage(arguments)
		if argumentsFile != "" {
			var r io.Reader = os.Stdin
			if argumentsFile != "-" {
				f, err := os.Open(argumentsFile)
				if err != nil {
					return err
				}
				defer f.Close()
				r = f
			}
			req.Arguments, err = io.ReadAll(io.LimitReader(r, 1<<20))
			if err != nil {
				return err
			}
		}
	}
	if act && !openCU {
		if actions != "" && actionsFile != "" {
			return errors.New("use either --actions or --actions-file")
		}
		raw := []byte(actions)
		if actionsFile != "" {
			var r io.Reader = os.Stdin
			if actionsFile != "-" {
				f, err := os.Open(actionsFile)
				if err != nil {
					return err
				}
				defer f.Close()
				r = f
			}
			var err error
			raw, err = io.ReadAll(io.LimitReader(r, 1<<20))
			if err != nil {
				return err
			}
		}
		if err := decodeActions(raw, &req.Actions); err != nil {
			return err
		}
	} else if region != "" {
		r, err := parseDesktopRegion(region)
		if err != nil {
			return err
		}
		req.Region = &r
	}
	if err := desktop.Validate(name, &req); err != nil {
		return err
	}
	c, err := client.New()
	if err != nil {
		return err
	}
	var res *client.DesktopResponse
	if openCU {
		res, err = c.ActWorkspace(ctx, ref, req)
	} else if act {
		res, err = c.Act(ctx, *target, req)
	} else {
		res, err = c.Screenshot(ctx, *target, via, req)
	}
	if res != nil {
		// A lost image after an act header still contains useful completed-action
		// evidence. Preserve it, but never disguise an incomplete exchange as done.
		if err != nil {
			res.Result.Status = "unknown"
			res.Result.Error = err.Error()
		}
		if outputErr := writeDesktopOutput(res, *target, *output, os.Stdout, os.Stderr); outputErr != nil {
			return outputErr
		}
	}
	if err != nil {
		return err
	}
	if res.Result.OpenCU != nil && len(res.Result.OpenCU.Result) > 0 {
		var r struct {
			IsError bool `json:"isError"`
		}
		_ = json.Unmarshal(res.Result.OpenCU.Result, &r)
		if r.IsError {
			return errors.New("OpenCU tool returned isError; inspect the result; do not retry automatically")
		}
	}
	if res.Result.Status != "completed" {
		return errors.New(res.Result.Error)
	}
	return nil
}
func decodeActions(raw []byte, actions *[]protocol.DesktopAction) error {
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if err := d.Decode(actions); err != nil {
		return errors.New("invalid actions JSON; see wanctl help act")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("actions must contain one JSON array")
	}
	return nil
}
func parseDesktopRegion(s string) (protocol.Rect, error) {
	fields := strings.Split(s, ",")
	var n [4]int
	if len(fields) != 4 {
		return protocol.Rect{}, errors.New("region must be x,y,width,height")
	}
	for i, v := range fields {
		var err error
		n[i], err = strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return protocol.Rect{}, errors.New("region needs four integers")
		}
	}
	return protocol.Rect{X: n[0], Y: n[1], Width: n[2], Height: n[3]}, nil
}
func writeDesktopOutput(res *client.DesktopResponse, target, path string, stdout, stderr io.Writer) error {
	if res.Result.OpenCU != nil {
		return writeOpenCUOutput(res.Result, path, stdout, stderr)
	}
	out := struct {
		protocol.DesktopResult
		Path   string `json:"path,omitempty"`
		Legacy bool   `json:"legacy,omitempty"`
	}{DesktopResult: res.Result, Legacy: res.Legacy}
	if len(res.Image) > 0 {
		if path == "-" {
			if err := json.NewEncoder(stderr).Encode(out); err != nil {
				return err
			}
			_, err := stdout.Write(res.Image)
			return err
		}
		if path == "" {
			ext := "jpg"
			if res.Legacy {
				ext = "png"
			}
			if target == "" {
				target = "device"
			}
			target = strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(target)
			path = fmt.Sprintf("screenshot-%s-%s.%s", target, time.Now().Format("20060102-150405.000"), ext)
		}
		// Captured screens can contain personal information. Keep them private.
		if err := os.WriteFile(path, res.Image, 0o600); err != nil {
			return err
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return err
		}
		out.Path = path
	}
	return json.NewEncoder(stdout).Encode(out)
}

// The full raw MCP result is kept in a private JSON file; stdout prints text,
// image sizes and metadata without dumping megabytes of base64 into a terminal.
func writeOpenCUOutput(res protocol.DesktopResult, path string, stdout, stderr io.Writer) error {
	r := res.OpenCU
	if opencu.ErrorCode(r.Result) == "PERSON_ACTIVE" {
		if _, err := fmt.Fprintln(stderr, "Stop and ask your user; never retry automatically."); err != nil {
			return err
		}
	}
	if len(r.Discovery) > 0 {
		_, err := fmt.Fprintln(stdout, string(r.Discovery))
		return err
	}
	if len(r.Result) == 0 {
		return json.NewEncoder(stdout).Encode(r)
	}
	if path == "-" {
		_, err := fmt.Fprintln(stdout, string(r.Result))
		return err
	}
	if path == "" {
		f, err := os.CreateTemp(".", "opencu-result-*.json")
		if err != nil {
			return err
		}
		path = f.Name()
		if err = f.Close(); err != nil {
			return err
		}
	}
	if err := os.WriteFile(path, r.Result, 0600); err != nil {
		return err
	}
	if err := os.Chmod(path, 0600); err != nil {
		return err
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(r.Result, &result); err != nil {
		return err
	}
	var content []map[string]json.RawMessage
	if err := json.Unmarshal(result["content"], &content); err != nil {
		return err
	}
	for _, item := range content {
		var kind string
		_ = json.Unmarshal(item["type"], &kind)
		if kind == "image" {
			var data string
			_ = json.Unmarshal(item["data"], &data)
			n := base64.StdEncoding.DecodedLen(len(data))
			if strings.HasSuffix(data, "==") {
				n -= 2
			} else if strings.HasSuffix(data, "=") {
				n--
			}
			delete(item, "data")
			item["bytes"], _ = json.Marshal(n)
		}
	}
	result["content"], _ = json.Marshal(content)
	absolute, _ := filepath.Abs(path)
	result["result_file"], _ = json.Marshal(absolute)
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return err
	}
	return nil
}
