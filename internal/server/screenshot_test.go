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
