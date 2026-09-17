package client

import (
	"bytes"
	"context"
	"image/png"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"wanctl/internal/policy"
	"wanctl/internal/protocol"
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
	// No rule is granted and none is needed: a desktop capture is gated as the
	// ordinary command it is, which bypass mode covers. A device that demanded
	// an elevated rule here would be refusing a capability it has already given
	// this controller.
	c, ctx := startDevice(t, policy.ModeBypass)

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

// The exec path — which is how a screenshot travels — never made the mistake
// the file path did: a connection that ends mid-command is reported as what it
// is, not as an agent that cannot run the command. Asserted so the new
// screenshot tool cannot inherit it later.
func TestLostExecConnectionIsNotAnUnsupportedAgent(t *testing.T) {
	device, controller := net.Pipe()
	defer controller.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The device receives the command — and may well be running it — then
		// the session ends without an exit frame ever arriving.
		protocol.ReadMessage(device)
		device.Close()
	}()

	req := ExecRequest{Command: "screenshot", Elevate: true, ElevateOptional: true}
	if err := protocol.WriteMessage(controller, protocol.Message{
		Kind: protocol.KindExec, Command: req.Command, OneShot: true, Elevate: req.Elevate,
	}); err != nil {
		t.Fatal(err)
	}
	<-done

	var out bytes.Buffer
	res, err := execOver(context.Background(), controller, req, &out, &out)
	if err == nil {
		t.Fatal("a dropped connection was reported as success")
	}
	if res.Code != -1 {
		t.Errorf("exit code = %d, want -1 for a command whose result never arrived", res.Code)
	}
	for _, forbidden := range []string{"does not support", "wanctl update"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("message = %q, which blames the agent's version", err.Error())
		}
	}
}

// A command that fails after emitting megabytes is exactly when knowing where
// the rest of the output went matters most, so the error frame carries the
// spill and the controller carries it out.
func TestSpillSurvivesAnErrorFrame(t *testing.T) {
	device, controller := net.Pipe()
	defer controller.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		protocol.ReadMessage(device)
		// Output, then a failure — the shape of a build that died mid-run.
		protocol.WriteFrame(device, protocol.FrameStdout, bytes.Repeat([]byte("x"), 100))
		protocol.WriteMessage(device, protocol.Message{
			Kind:   protocol.KindError,
			Reason: "command cancelled by the controller",
			Path:   "/tmp/wanctl-exec-deadbeef.log", Size: 4096, SpillKept: 4096,
		})
	}()

	req := ExecRequest{Command: "make", OneShot: true, SpillAfter: 64}
	if err := protocol.WriteMessage(controller, protocol.Message{Kind: protocol.KindExec, Command: req.Command}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	res, err := execOver(context.Background(), controller, req, &out, &out)
	<-done

	if err == nil {
		t.Fatal("an error frame was reported as success")
	}
	if res.SpillPath != "/tmp/wanctl-exec-deadbeef.log" {
		t.Errorf("the spill path was dropped on the error path: %q", res.SpillPath)
	}
	if res.SpillBytes != 4096 || res.SpillKept != 4096 {
		t.Errorf("byte counts lost: %d/%d", res.SpillKept, res.SpillBytes)
	}
	if res.Code != -1 {
		t.Errorf("code = %d, want -1", res.Code)
	}
}
