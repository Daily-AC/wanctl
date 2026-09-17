package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// This file gives the `screenshot` verb a body on the three desktop platforms.
//
// On Android the verb is a translation of `screencap -p` and lives in
// internal/androidverb, where it has to go through the elevation channel to see
// anything at all. A desktop has no such channel and no single capture tool
// either: macOS ships screencapture, Windows can be asked to blit the virtual
// screen through .NET, and a Linux box has whichever of grim, gnome-screenshot
// or import its owner installed. What they share is the shape — one PNG on
// stdout — so the wire shape stays what it was: an exec whose output happens to
// be an image, and every caller that already knew `wanctl screenshot` keeps
// working when the device turns out to be a laptop.
//
// Nothing here writes a file the caller keeps. The capture tools all insist on
// writing to a path, so the PNG lands in a temp file, is copied to the stream
// and is removed before this returns.

// screenshotVerb is the command a controller sends for a capture.
const screenshotVerb = "screenshot"

// IsDesktopCapture reports whether this command is a screen capture that needs
// no elevation channel — the verb, on a device that is not Android.
//
// The agent asks before it gates, not after. A capture is requested elevated on
// every platform, because Android cannot do it any other way and the controller
// cannot know which kind of device it is talking to; but on a laptop nothing is
// elevated, and gating it as though something were would put a capture behind a
// stricter rule than the `screencapture` a controller with exec permission can
// already run for itself. Same capability, so the same gate.
func IsDesktopCapture(command string) bool {
	return strings.TrimSpace(command) == screenshotVerb && runtime.GOOS != "android"
}

// RunScreenshot captures the device's screen and writes a PNG to out.
//
// handled is false when command is not the screenshot verb, or when this is an
// Android agent, where the verb belongs to the elevated path instead. The
// caller treats that exactly as it treats any other command.
func RunScreenshot(ctx context.Context, command string, out io.Writer) (handled bool, code int, err error) {
	if !IsDesktopCapture(command) {
		return false, 0, nil
	}
	png, err := captureScreen(ctx)
	if err != nil {
		return true, 0, err
	}
	_, err = out.Write(png)
	return true, 0, err
}

// captureScreen returns the PNG bytes of the whole desktop.
func captureScreen(ctx context.Context) ([]byte, error) {
	f, err := os.CreateTemp("", "wanctl-screenshot-*.png")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	if err := captureTo(ctx, path); err != nil {
		return nil, err
	}
	png, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(png) == 0 {
		return nil, fmt.Errorf("the capture tool produced an empty file; on macOS this usually means this agent has not been granted Screen Recording permission in System Settings → Privacy & Security")
	}
	if !bytes.HasPrefix(png, []byte("\x89PNG\r\n\x1a\n")) {
		return nil, fmt.Errorf("the capture tool produced %d bytes that are not a PNG", len(png))
	}
	return png, nil
}

// captureTo runs this platform's capture tool, writing a PNG to path.
func captureTo(ctx context.Context, path string) error {
	switch runtime.GOOS {
	case "darwin":
		// -x is the whole point on a machine someone may be sitting at: no
		// shutter sound, no flash, nothing that says a remote agent just looked.
		// Except that a person should be able to tell, so the event log records
		// the capture like every other command; silencing the OS chrome is
		// about not startling, not about hiding.
		return runCapture(ctx, "screencapture", "-x", "-t", "png", path)
	case "windows":
		return runCapture(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", windowsCapture(path))
	default:
		return captureUnix(ctx, path)
	}
}

// captureUnix tries the Linux capture tools in turn: grim on Wayland,
// gnome-screenshot on a GNOME session, ImageMagick's import on plain X11.
func captureUnix(ctx context.Context, path string) error {
	candidates := []struct {
		tool string
		args []string
	}{
		{"grim", []string{path}},
		{"gnome-screenshot", []string{"-f", path}},
		{"import", []string{"-window", "root", path}},
	}
	var tried []string
	for _, c := range candidates {
		if _, err := exec.LookPath(c.tool); err != nil {
			tried = append(tried, c.tool)
			continue
		}
		return runCapture(ctx, c.tool, c.args...)
	}
	return fmt.Errorf("no screen capture tool on this device: install grim (Wayland), gnome-screenshot (GNOME) or ImageMagick's import (X11). Looked for %s",
		strings.Join(tried, ", "))
}

// runCapture runs one capture tool and turns a failure into a message that says
// which tool failed and what it said, rather than an exit code on its own.
func runCapture(ctx context.Context, tool string, args ...string) error {
	cmd := exec.CommandContext(ctx, tool, args...)
	hideConsole(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s failed: %s", filepath.Base(tool), msg)
	}
	return nil
}

// windowsCapture is the PowerShell that blits the virtual screen — every
// monitor, in their desktop arrangement — into one PNG. It uses only what ships
// with Windows, so nothing has to be installed on the device first.
func windowsCapture(path string) string {
	return "Add-Type -AssemblyName System.Windows.Forms,System.Drawing; " +
		"$b = [System.Windows.Forms.SystemInformation]::VirtualScreen; " +
		"$bmp = New-Object System.Drawing.Bitmap($b.Width, $b.Height); " +
		"$g = [System.Drawing.Graphics]::FromImage($bmp); " +
		"$g.CopyFromScreen($b.X, $b.Y, 0, 0, $bmp.Size); " +
		"$bmp.Save(" + quotePowerShellLiteral(path) + ", [System.Drawing.Imaging.ImageFormat]::Png); " +
		"$g.Dispose(); $bmp.Dispose()"
}
