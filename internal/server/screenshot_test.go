package server

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"wanctl/internal/desktop"
	"wanctl/internal/protocol"
)

// The verb is only this device's verb. Anything else is an ordinary command and
// must reach the shell untouched.
func TestRunScreenshotOnlyHandlesItsOwnVerb(t *testing.T) {
	for _, cmd := range []string{"echo screenshot", "screenshot -o x.png", "ls"} {
		handled, _, err := RunScreenshot(context.Background(), cmd, &bytes.Buffer{})
		if handled {
			t.Errorf("%q was taken as a capture", cmd)
		}
		if err != nil {
			t.Errorf("%q: %v", cmd, err)
		}
	}
}

// The real capture on this machine. A PNG that decodes with real dimensions is
// the whole assertion: everything above it is shape, and this is the part that
// only a machine with a screen can answer.
func TestCaptureScreenProducesADecodablePNG(t *testing.T) {
	if os.Getenv("WANCTL_TEST_SCREEN_CAPTURE") != "1" {
		t.Skip("requires supervised WANCTL_TEST_SCREEN_CAPTURE=1")
	}
	if testing.Short() {
		t.Skip("drives the OS capture tool")
	}
	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("screencapture"); err != nil {
			t.Skip("screencapture is not on PATH")
		}
	case "windows":
	default:
		if _, err := exec.LookPath("grim"); err != nil {
			if _, err := exec.LookPath("import"); err != nil {
				t.Skip("no capture tool installed on this machine")
			}
		}
	}
	raw, err := captureScreen(context.Background())
	if err != nil {
		// A build machine with no display, or a Mac that has not granted
		// Screen Recording, cannot answer this. That is not a failure of the
		// code under test.
		t.Skipf("no capture available here: %v", err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("captured %d bytes that do not decode as PNG: %v", len(raw), err)
	}
	if cfg.Width == 0 || cfg.Height == 0 {
		t.Fatalf("captured a %dx%d image", cfg.Width, cfg.Height)
	}
	t.Logf("captured %dx%d, %d bytes", cfg.Width, cfg.Height, len(raw))
}

// A Linux device with none of the tools installed has to say so by name, so its
// owner knows what to install instead of reading "capture failed".
func TestUnixCaptureNamesTheToolsToInstall(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		t.Skip("the fallback chain is the Linux path")
	}
	if _, err := exec.LookPath("grim"); err == nil {
		t.Skip("a capture tool is installed here, so there is no refusal to inspect")
	}
	err := captureUnix(context.Background(), t.TempDir()+"/out.png")
	if err == nil {
		t.Skip("something captured after all")
	}
	for _, tool := range []string{"grim", "gnome-screenshot", "import"} {
		if !strings.Contains(err.Error(), tool) {
			t.Errorf("refusal does not name %s: %v", tool, err)
		}
	}
}

// The failure a Mac reports when the agent has no Screen Recording permission.
// screencapture says only that the display would not give it an image, so the
// agent has to recognise that line and say what it means; otherwise the person
// reads "could not create image from display" and has nothing to act on.
//
// These are the captured stderr strings, not a real capture: denying TCC to
// this test process is not something a test can do.
func TestScreencaptureDenialExplainsScreenRecording(t *testing.T) {
	for _, stderr := range []string{
		"screencapture: could not create image from display 1",
		"screencapture: cannot create image from display",
	} {
		err := captureFailure("/usr/sbin/screencapture", stderr)
		if err == nil {
			t.Fatalf("%q was not treated as a failure", stderr)
		}
		got := err.Error()
		if !strings.Contains(got, stderr) {
			t.Errorf("the original line is gone from %q", got)
		}
		for _, want := range []string{
			"Screen Recording",
			"System Settings",
			"Privacy & Security",
			"restart the agent",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("the remedy does not say %q: %s", want, got)
			}
		}
	}
}

// Every other capture failure keeps the tool's own words and gains nothing:
// a permission remedy on an unrelated error sends the reader the wrong way.
func TestOtherCaptureFailuresAreNotBlamedOnPermissions(t *testing.T) {
	cases := map[string]string{
		"/usr/sbin/screencapture": "screencapture: cannot write file to intended destination",
		"grim":                    "compositor does not support wlr-screencopy",
	}
	for tool, stderr := range cases {
		got := captureFailure(tool, stderr).Error()
		if !strings.Contains(got, stderr) {
			t.Errorf("%s: the original line is gone from %q", tool, got)
		}
		if strings.Contains(got, "Screen Recording") {
			t.Errorf("%s: an unrelated failure was blamed on permissions: %s", tool, got)
		}
	}
}

// powershell.exe is not DPI aware, so on a scaled display VirtualScreen comes
// back in logical pixels and the capture is the top-left corner of the screen.
// The script has to make the process aware first, and before it reads the size,
// or the size it reads is still the scaled one.
func TestWindowsCaptureDeclaresDPIAwarenessBeforeMeasuring(t *testing.T) {
	script := windowsCapture(`C:\Temp\shot.png`)
	measure := strings.Index(script, "VirtualScreen")
	if measure < 0 {
		t.Fatalf("script no longer reads VirtualScreen:\n%s", script)
	}
	for _, call := range []string{
		"SetProcessDpiAwarenessContext([IntPtr](-4))", // per-monitor aware v2
		"SetProcessDPIAware()",                        // fallback before Windows 10 1703
	} {
		at := strings.Index(script, "]::"+call)
		if at < 0 {
			t.Errorf("script does not call %s:\n%s", call, script)
			continue
		}
		if at > measure {
			t.Errorf("script calls %s after it reads VirtualScreen", call)
		}
	}
}

func TestSessionZeroLegacyScreenshotUsesHelper(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 16, 9))
	img.Set(4, 3, color.RGBA{200, 40, 30, 255})
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, nil); err != nil {
		t.Fatal(err)
	}
	calls := 0
	helper := func(ctx context.Context, job desktop.Job) (protocol.DesktopResult, []byte, error) {
		calls++
		if job.Action != "screenshot" || job.Reference != nil || len(job.Request.Actions) != 0 {
			t.Fatalf("unexpected job: %+v", job)
		}
		desktop.RecordSession(ctx, 9)
		return protocol.DesktopResult{Status: "completed", Snapshot: &protocol.DesktopSnapshot{Session: 9}}, encoded.Bytes(), nil
	}
	var recorded uint32
	ctx := desktop.WithSessionObserver(context.Background(), func(id uint32) { recorded = id })
	data, err := captureScreenUsing(ctx, func() (bool, error) { return true, nil }, helper, func(context.Context) ([]byte, error) {
		t.Fatal("session 0 used the blank PowerShell capture")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width != 16 || cfg.Height != 9 || calls != 1 || recorded != 9 {
		t.Fatalf("cfg=%+v err=%v calls=%d session=%d", cfg, err, calls, recorded)
	}
}

func TestLegacyDesktopSessionCaptureUnchanged(t *testing.T) {
	original := []byte("original PNG bytes")
	data, err := captureScreenUsing(context.Background(), func() (bool, error) { return false, nil }, func(context.Context, desktop.Job) (protocol.DesktopResult, []byte, error) {
		t.Fatal("desktop-session legacy capture was rerouted")
		return protocol.DesktopResult{}, nil, nil
	}, func(context.Context) ([]byte, error) { return original, nil })
	if err != nil || !bytes.Equal(data, original) {
		t.Fatalf("%q %v", data, err)
	}
}

func TestSessionZeroLegacyScreenshotPreservesRefusal(t *testing.T) {
	for _, cause := range []string{"desktop unavailable: no signed-in user at the active console", "desktop unavailable: screen locked or secure desktop active"} {
		data, err := captureScreenUsing(context.Background(), func() (bool, error) { return true, nil }, func(context.Context, desktop.Job) (protocol.DesktopResult, []byte, error) {
			return protocol.DesktopResult{Status: "rejected", Error: cause}, nil, nil
		}, func(context.Context) ([]byte, error) { t.Fatal("fell back to blank screen"); return nil, nil })
		if len(data) != 0 || err == nil || !strings.Contains(err.Error(), cause) {
			t.Fatalf("%q %v", data, err)
		}
	}
}
