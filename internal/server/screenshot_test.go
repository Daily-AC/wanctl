package server

import (
	"bytes"
	"context"
	"image/png"
	"os/exec"
	"runtime"
	"strings"
	"testing"
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
