package main

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"wanctl/internal/agent"
	"wanctl/internal/client"
	"wanctl/internal/policy"
	"wanctl/internal/relay"
	"wanctl/internal/transport"
)

// hostile is what a device controlled by someone else might send: a sequence
// that clears the screen and prints a fake prompt, a window-title sequence, a
// C1 control spelled in UTF-8, and a lone 8-bit CSI — mixed with text that has
// to survive untouched.
const hostile = "before\x1b[2J\x1b[Hfake$ \x1b]0;t\x07 \u009b31m \x9b 你好\tend\r\n"

// On a terminal, device output is escaped; into a pipe or a file it keeps
// every byte. The decision is per stream, so a redirected stdout stays exact
// while stderr is still on the terminal.
func TestDeviceOutputEscapesOnlyForATerminal(t *testing.T) {
	for _, tty := range []bool{true, false} {
		f, err := os.CreateTemp(t.TempDir(), "out")
		if err != nil {
			t.Fatal(err)
		}
		isTerminal = func(*os.File) bool { return tty }
		w, flush := deviceOutput(f)
		w.Write([]byte(hostile[:10]))
		w.Write([]byte(hostile[10:]))
		flush()
		f.Close()
		got, _ := os.ReadFile(f.Name())
		if !tty {
			if !bytes.Equal(got, []byte(hostile)) {
				t.Fatalf("non-terminal output changed:\n got %q\nwant %q", got, hostile)
			}
			continue
		}
		if bytes.ContainsAny(got, "\x1b\x07") || bytes.Contains(got, []byte("\u009b")) || bytes.IndexByte(got, 0x9b) >= 0 {
			t.Fatalf("terminal output still carries control characters: %q", got)
		}
		for _, keep := range []string{"before", `\x1b[2J`, "fake$ ", `\u009b31m`, `\x9b`, "你好\tend\r\n"} {
			if !bytes.Contains(got, []byte(keep)) {
				t.Fatalf("terminal output lost %q: %q", keep, got)
			}
		}
	}
	isTerminal = func(f *os.File) bool { return false }
}

// deviceHarness is a relay and a bypass-mode agent in this process, with the
// environment set up for a compiled controller binary to reach it.
func deviceHarness(t *testing.T) (ctx context.Context, target string) {
	t.Helper()
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("termout:alice")).Handler())
	t.Cleanup(srv.Close)
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	ag, err := agent.New(agent.Options{RelayURL: srv.URL, Token: "termout", Name: "term-device", AutoYes: true, Mode: policy.ModeBypass, Transport: "http"})
	if err != nil {
		t.Fatal(err)
	}
	deviceID, err := transport.LoadOrCreateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	t.Cleanup(ag.Close)
	go ag.Run(ctx)
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", srv.URL)
	t.Setenv("WANCTL_TOKEN", "termout")
	t.Setenv("WANCTL_TRANSPORT", "http")
	t.Setenv("WANCTL_WORKSPACE", "")
	t.Setenv("WANCTL_LABEL", "termout test")
	c, err := client.New()
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		peers, e := c.Peers(ctx)
		if e == nil && len(peers) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("registration: %v", e)
		}
		time.Sleep(10 * time.Millisecond)
	}
	target = "alice/" + ag.DeviceID()
	if _, err := c.PinServer(ctx, target, deviceID.Fingerprint, false); err != nil {
		t.Fatal(err)
	}
	return ctx, target
}

// printfArg writes hostile as a printf format the device's shell understands.
func printfArg() string {
	var b strings.Builder
	for _, c := range []byte(hostile) {
		switch {
		case c == '%':
			b.WriteString("%%")
		case c < 0x20 || c >= 0x7f || c == '\'' || c == '\\':
			b.WriteString(`\` + string('0'+(c>>6)) + string('0'+((c>>3)&7)) + string('0'+(c&7)))
		default:
			b.WriteByte(c)
		}
	}
	return "printf '" + b.String() + "'"
}

// Through the compiled binary, with stdout and stderr as pipes: exec output
// arrives byte for byte, and a device's error text is escaped all the same.
func TestExecOutputIntoAPipeIsExact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("device side uses /bin/sh")
	}
	bin := buildWanctl(t)
	ctx, target := deviceHarness(t)

	cmd := exec.CommandContext(ctx, bin, "exec", "--target", target, "--oneshot", printfArg())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("exec: %v\n%s", err, &stderr)
	}
	if !bytes.Equal(stdout.Bytes(), []byte(hostile)) {
		t.Fatalf("piped output changed:\n got %q\nwant %q", stdout.Bytes(), hostile)
	}

	// The device answers a read of a missing file with an error that repeats
	// the path, so the device's own error text carries the sequence back.
	cmd = exec.CommandContext(ctx, bin, "read", "--target", target, "/nonexistent\x1b[2J\x1b]0;t\x07")
	stdout.Reset()
	stderr.Reset()
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	var exit *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &exit) {
		t.Fatalf("read of a missing file: %v", err)
	}
	if bytes.ContainsAny(stderr.Bytes(), "\x1b\x07") || !strings.Contains(stderr.String(), `\x1b[2J`) {
		t.Fatalf("device error text reached stderr unescaped: %q", stderr.String())
	}
}

// Through the compiled binary on a real pseudo-terminal: the same output is
// shown escaped. `script` allocates the terminal; everything it prints is what
// the terminal received.
func TestExecOutputOnATerminalIsEscaped(t *testing.T) {
	scriptBin, err := exec.LookPath("script")
	if err != nil || (runtime.GOOS != "darwin" && runtime.GOOS != "linux") {
		t.Skip("needs script(1) to allocate a pseudo-terminal")
	}
	bin := buildWanctl(t)
	ctx, target := deviceHarness(t)
	args := []string{bin, "exec", "--target", target, "--oneshot", printfArg()}
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		cmd = exec.CommandContext(ctx, scriptBin, append([]string{"-q", "/dev/null"}, args...)...)
	} else {
		quoted := make([]string, len(args))
		for i, a := range args {
			quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		cmd = exec.CommandContext(ctx, scriptBin, "-qec", strings.Join(quoted, " "), "/dev/null")
	}
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("script could not run the controller on a terminal: %v", err)
	}
	if bytes.ContainsAny(out, "\x1b\x07") || bytes.Contains(out, []byte("\u009b")) {
		t.Fatalf("terminal received control characters: %q", out)
	}
	for _, keep := range []string{"before", `\x1b[2J`, `\u009b31m`, "你好"} {
		if !bytes.Contains(out, []byte(keep)) {
			t.Fatalf("terminal output lost %q: %q", keep, out)
		}
	}
}

func TestErrorTextIsEscaped(t *testing.T) {
	err := &client.RejectError{Reason: "no\x1b[2J", PairingURL: "https://p.example/#pair\x1b]0;x\x07"}
	if got := errorText(err); strings.ContainsAny(got, "\x1b\x07") {
		t.Fatalf("errorText kept control characters: %q", got)
	}
	if got := errorText(errors.New("remote error: \rwanctl: ok")); strings.Contains(got, "\r") {
		t.Fatalf("a bare carriage return survived: %q", got)
	}
}
