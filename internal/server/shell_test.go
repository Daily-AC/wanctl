package server

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func specialCwd(t *testing.T) string {
	t.Helper()
	cwd := filepath.Join(t.TempDir(), `dir with spaces";semi'quote`)
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	return cwd
}

func TestRunOneShotCwdWithSpecialCharacters(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	cwd := specialCwd(t)

	var out bytes.Buffer
	code, err := RunOneShot("/bin/sh", "pwd", cwd, &out)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, output = %q", code, out.String())
	}
	if got := strings.TrimSpace(out.String()); got != cwd {
		t.Fatalf("pwd = %q, want %q", got, cwd)
	}
}

// A one-shot must report the same exit code the session path reports. The
// session path reads $LASTEXITCODE in its end-of-command marker; the one-shot
// path relies on the process exit code, which `powershell -Command` derives from
// $? (0 or 1) unless the source ends in an explicit exit.
func TestWinExitEpilogueMirrorsSessionMarker(t *testing.T) {
	if !strings.HasPrefix(winExitEpilogue, "\n") {
		t.Error("epilogue must start on its own line, or a trailing comment in the command swallows it")
	}
	for _, want := range []string{"exit ", "$LASTEXITCODE", "[int](-not $?)"} {
		if !strings.Contains(winExitEpilogue, want) {
			t.Errorf("epilogue missing %q — it must mirror the session marker's expression", want)
		}
	}
}

// PowerShell decodes a session's stdin in the console's OEM code page, so
// whatever a command contains, a Windows session must be sent printable ASCII.
// It must also arrive as exactly two lines, the reset and the command, because
// the session runs each line of stdin as it is read.
func TestWinSessionCommandIsPrintableASCII(t *testing.T) {
	for _, command := range []string{
		"echo 中文",
		"Write-Output 'it''s ünïcödé 😀'",
		"if ($true) {\r\n\t'multi-line'\r\n}",
		"",
	} {
		src := winSessionCommand(command)
		lines := strings.Split(src, "\n")
		if len(lines) != 2 {
			t.Fatalf("%q is sent as %d lines, want 2: %q", command, len(lines), src)
		}
		for _, line := range lines {
			for i := 0; i < len(line); i++ {
				if c := line[i]; c < 0x20 || c > 0x7e {
					t.Fatalf("%q is sent with byte %#x: %q", command, c, line)
				}
			}
		}
	}
}

func TestRunOneShotPropagatesNativeExitCode(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("exercises the PowerShell one-shot path")
	}
	var out bytes.Buffer
	code, err := RunOneShot("powershell", "cmd /c exit 7", "", &out)
	if err != nil {
		t.Fatal(err)
	}
	if code != 7 {
		t.Fatalf("exit code = %d, want 7 (output %q)", code, out.String())
	}
}

func TestShellSessionExecInDirPersistsCwd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	cwd := specialCwd(t)
	s, err := NewShellSession("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	var first bytes.Buffer
	code, err := s.ExecInDir("pwd", cwd, &first)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || strings.TrimSpace(first.String()) != cwd {
		t.Fatalf("first pwd: code=%d output=%q, want %q", code, first.String(), cwd)
	}

	var next bytes.Buffer
	code, err = s.Exec("pwd", &next)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || strings.TrimSpace(next.String()) != cwd {
		t.Fatalf("persistent pwd: code=%d output=%q, want %q", code, next.String(), cwd)
	}
}

func TestChangeDirCommandUsesPlatformDataFileProtocol(t *testing.T) {
	const dataPath = `/tmp/wanctl-cwd-'quote"double;semi`
	posix := changeDirCommand("linux", dataPath)
	if strings.Contains(posix, "Set-Location") || !strings.Contains(posix, "IFS= read -r WANCTL_CWD") {
		t.Fatalf("unexpected POSIX cwd protocol: %q", posix)
	}
	powershell := changeDirCommand("windows", dataPath)
	if !strings.Contains(powershell, "ReadAllText") || !strings.Contains(powershell, "Set-Location -LiteralPath $wanctlCwd") {
		t.Fatalf("unexpected PowerShell cwd protocol: %q", powershell)
	}
	const cwd = `dir with spaces";semi'quote`
	if strings.Contains(posix, cwd) || strings.Contains(powershell, cwd) {
		t.Fatal("cwd contents must not be included in the shell protocol")
	}
}
