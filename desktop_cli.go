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
	output := fs.String("o", "", "legacy image file or OpenCU output directory; - writes the complete result to stdout")
	id := fs.String("screenshot-id", "", "screenshot coordinate reference")
	var via, region, actions, actionsFile, tool, arguments, argumentsFile string
	var discover bool
	if act {
		fs.BoolVar(&discover, "discover", false, "discover OpenCU version, instructions and tools")
		fs.StringVar(&tool, "tool", "", "OpenCU tool name from discovery")
		fs.StringVar(&arguments, "arguments", "", "OpenCU arguments JSON object")
		fs.StringVar(&argumentsFile, "arguments-file", "", "arguments JSON file (- for stdin)")
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
	openCU := discover || tool != "" || arguments != "" || argumentsFile != ""
	if openCU {
		if actions != "" || actionsFile != "" {
			return errors.New("OpenCU mode is mutually exclusive with actions")
		}
		var err error
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
	if act {
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

// Keep the raw result and decode every image into its own private PNG file.
// Text and metadata stay in stdout JSON, with paths replacing base64 payloads.
func writeOpenCUOutput(res protocol.DesktopResult, dir string, stdout, stderr io.Writer) error {
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
	if dir == "-" {
		_, err := fmt.Fprintln(stdout, string(r.Result))
		return err
	}
	var err error
	if dir == "" {
		dir, err = os.MkdirTemp("", "wanctl-opencu-")
	} else {
		err = os.MkdirAll(dir, 0700)
	}
	if err != nil {
		return err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	resultPath, err := writeOpenCUFile(dir, "opencu-result-*.json", r.Result)
	if err != nil {
		return err
	}
	var result map[string]json.RawMessage
	if err = json.Unmarshal(r.Result, &result); err != nil {
		return err
	}
	var content []map[string]json.RawMessage
	if err = json.Unmarshal(result["content"], &content); err != nil {
		return err
	}
	for _, item := range content {
		var kind string
		_ = json.Unmarshal(item["type"], &kind)
		if kind != "image" {
			continue
		}
		var encoded, mime string
		if json.Unmarshal(item["data"], &encoded) != nil || json.Unmarshal(item["mimeType"], &mime) != nil || mime != "image/png" {
			return fmt.Errorf("invalid OpenCU PNG content; raw result saved to %s", resultPath)
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(data) < 8 || string(data[:8]) != "\x89PNG\r\n\x1a\n" {
			return fmt.Errorf("invalid OpenCU PNG data; raw result saved to %s", resultPath)
		}
		imagePath, err := writeOpenCUFile(dir, "opencu-image-*.png", data)
		if err != nil {
			return err
		}
		delete(item, "data")
		item["path"], _ = json.Marshal(imagePath)
		item["bytes"], _ = json.Marshal(len(data))
	}
	result["content"], _ = json.Marshal(content)
	result["result_file"], _ = json.Marshal(resultPath)
	return json.NewEncoder(stdout).Encode(result)
}

// CreateTemp provides unique names and 0600 permissions even in an existing
// output directory. Never overwrite an earlier call's image or follow a symlink.
func writeOpenCUFile(dir, pattern string, data []byte) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	path := f.Name()
	if _, err = f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	if err = f.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}
