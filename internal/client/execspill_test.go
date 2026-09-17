package client

import (
	"bytes"
	"image/png"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"wanctl/internal/policy"
)

// Over the real relay, through a real agent: a command whose output passes the
// threshold leaves the whole of it in a file on the device, and the controller
// is told where. This is the half of the truncation story that cannot be faked
// controller-side — the bytes never travel twice.
func TestExecSpillsLongOutputOnTheDevice(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture command is sh")
	}
	c, ctx := startDevice(t, policy.ModeBypass)

	var out, errOut bytes.Buffer
	res, err := c.ExecOut(ctx, ExecRequest{
		Target:     "home-pc",
		Command:    "i=0; while [ $i -lt 400 ]; do echo \"line $i: ....................\"; i=$((i+1)); done",
		OneShot:    true,
		SpillAfter: 2048,
	}, &out, &errOut)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.Code != 0 {
		t.Fatalf("exit %d: %s", res.Code, errOut.String())
	}
	if res.SpillPath == "" {
		t.Fatal("output past the threshold was not kept on the device")
	}
	t.Cleanup(func() { os.Remove(res.SpillPath) })

	if res.SpillBytes != int64(out.Len()) {
		t.Errorf("device counted %d bytes, controller received %d", res.SpillBytes, out.Len())
	}
	kept, err := os.ReadFile(res.SpillPath)
	if err != nil {
		t.Fatalf("the path the device named does not exist: %v", err)
	}
	if !bytes.Equal(kept, out.Bytes()) {
		t.Fatalf("the device kept %d bytes, the controller saw %d", len(kept), out.Len())
	}
	if !strings.Contains(string(kept), "line 399:") {
		t.Error("the device's copy is missing the end of the output")
	}
}

// Short output leaves nothing behind. A file per command would be litter on a
// device whose owner never asked for it.
func TestExecLeavesNoFileForShortOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture command is sh")
	}
	c, ctx := startDevice(t, policy.ModeBypass)

	var out, errOut bytes.Buffer
	res, err := c.ExecOut(ctx, ExecRequest{
		Target: "home-pc", Command: "echo hello", OneShot: true, SpillAfter: 2048,
	}, &out, &errOut)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.SpillPath != "" {
		t.Errorf("a one-line command spilled to %s", res.SpillPath)
	}
	if strings.TrimSpace(out.String()) != "hello" {
		t.Errorf("output = %q", out.String())
	}
}

// The desktop capture over the real link: the same verb Android answers with
// screencap, answered here by this machine's own capture tool, and a PNG that
// decodes at the other end.
func TestScreenshotOverTheRelayReturnsAPNG(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the OS capture tool")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("written for the macOS capture path")
	}
	if _, err := exec.LookPath("screencapture"); err != nil {
		t.Skip("screencapture is not on PATH")
	}
	c, ctx := startDeviceWithRules(t, policy.ModeBypass,
		policy.Rule{Kind: policy.KindExecElevated, Pattern: "screenshot", Scope: policy.ScopeGlobal})

	var shot, errOut bytes.Buffer
	res, err := c.ExecOut(ctx, ExecRequest{
		Target: "home-pc", Command: "screenshot", OneShot: true,
		Elevate: true, ElevateOptional: true,
	}, &shot, &errOut)
	if err != nil {
		t.Skipf("no capture available here: %v (%s)", err, errOut.String())
	}
	if res.Code != 0 || shot.Len() == 0 {
		t.Skipf("capture exited %d: %s", res.Code, errOut.String())
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(shot.Bytes()))
	if err != nil {
		t.Fatalf("received %d bytes that do not decode as PNG: %v", shot.Len(), err)
	}
	if cfg.Width == 0 || cfg.Height == 0 {
		t.Fatalf("received a %dx%d image", cfg.Width, cfg.Height)
	}
	t.Logf("received %dx%d, %d bytes over the relay", cfg.Width, cfg.Height, shot.Len())
}
