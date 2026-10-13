package server

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Text a controller sends must reach the session's PowerShell as the
// characters it was, not as their UTF-8 bytes read in the console's OEM code
// page: `echo 中文` printed `Σ╕¡µûç` on a code page 437 machine. A file the
// command names is checked as well as its output, because output alone also
// comes back right when input and output are mangled the same way.
func TestShellSessionCarriesNonASCIIText(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "目录")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := NewShellSession("powershell")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	const text = "中文 ünï 😀 it's"
	var out strings.Builder
	code, err := s.ExecInDirContext(ctx, "Write-Output "+quotePowerShellLiteral(text), "", &out)
	if err != nil || code != 0 || strings.TrimSpace(out.String()) != text {
		t.Fatalf("Write-Output: code=%d err=%v output=%q, want %q", code, err, out.String(), text)
	}
	code, err = s.ExecInDirContext(ctx, "Set-Content -LiteralPath '文件.txt' -Value x", dir, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("Set-Content: code=%d err=%v", code, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "文件.txt")); err != nil {
		t.Fatalf("the command did not create 文件.txt under its own name: %v", err)
	}
}

// Encoding the command must not change what the session reports or keeps: the
// exit code is the last statement's unless a native program set one, and
// variables and functions persist from one command to the next. A command is
// parsed as one unit, so a block spanning lines runs and a statement left open
// fails instead of waiting for more input.
func TestShellSessionExitCodesAndScope(t *testing.T) {
	s, err := NewShellSession("powershell")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	for _, c := range []struct {
		command string
		want    int
	}{
		{"Get-Item -LiteralPath 'C:\\wanctl-missing'", 1},
		{"Get-Item -LiteralPath 'C:\\wanctl-missing'; 'then fine'", 0},
		{"Write-Error boom", 1},
		{"throw 'boom'", 1},
		{"$wanctlTestVar = 'kept'; function wanctlTestFn { 'fn' }", 0},
		{"if ($wanctlTestVar -ne 'kept' -or (wanctlTestFn) -ne 'fn') { throw 'lost' }", 0},
		{"if ($true) {\r\n  'block'\r\n}", 0},
		{"if (", 1},
		// Last: $LASTEXITCODE stays set for every command after it.
		{"cmd /c exit 7", 7},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		var out strings.Builder
		code, err := s.ExecInDirContext(ctx, c.command, "", &out)
		cancel()
		if err != nil || code != c.want {
			t.Fatalf("%q: code=%d err=%v output=%q, want code %d", c.command, code, err, out.String(), c.want)
		}
	}
}
