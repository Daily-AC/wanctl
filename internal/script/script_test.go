package script

import (
	"encoding/base64"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestInterpForScript(t *testing.T) {
	cases := []struct {
		path string
		want Interp
		err  bool
	}{
		{"deploy.ps1", PowerShell, false},
		{"/tmp/Deploy.PS1", PowerShell, false},
		{"probe.sh", POSIX, false},
		{"probe.bash", POSIX, false},
		{"runme", POSIX, false},
		{"thing.py", "", true},
	}
	for _, c := range cases {
		got, err := ForPath(c.path)
		if c.err {
			if err == nil {
				t.Errorf("%s: want error, got %q", c.path, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.path, err)
		} else if got != c.want {
			t.Errorf("%s: got %q want %q", c.path, got, c.want)
		}
	}
}

// The point of -script is that the script's own syntax never reaches a shell.
// A script full of the exact characters that break inline commands must survive
// byte-for-byte.
func TestScriptCommandPowerShellRoundTrip(t *testing.T) {
	script := "Get-Process | Where-Object { $_.Name -ne $null } | ForEach-Object { \"名前: $($_.Name)\" }\n"
	cmd, err := Command(PowerShell, []byte(script))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cmd, "$_") || strings.Contains(cmd, "名") {
		t.Fatalf("script content leaked into the command line: %s", cmd)
	}
	enc := extractQuoted(t, cmd)
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeUTF16LE(raw)
	if !strings.HasPrefix(got, psPrologue) {
		t.Errorf("missing the progress-silencing prologue: %q", got)
	}
	if body := strings.TrimPrefix(got, psPrologue); body != script {
		t.Errorf("round trip mismatch:\n got %q\nwant %q", body, script)
	}
}

// A UTF-8 BOM on disk must not survive into the decoded script: PowerShell would
// see it as a stray token at the top of the file.
func TestScriptCommandStripsUTF8BOM(t *testing.T) {
	cmd, err := Command(PowerShell, append([]byte{0xEF, 0xBB, 0xBF}, []byte("Write-Output 'hi'")...))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(extractQuoted(t, cmd))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimPrefix(decodeUTF16LE(raw), psPrologue); got != "Write-Output 'hi'" {
		t.Errorf("got %q", got)
	}
}

func TestScriptCommandPOSIXRoundTrip(t *testing.T) {
	script := "#!/bin/sh\nfor f in *; do echo \"$f\"; done\n"
	cmd, err := Command(POSIX, []byte(script))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cmd, "$f") {
		t.Fatalf("script content leaked into the command line: %s", cmd)
	}
	raw, err := base64.StdEncoding.DecodeString(extractQuoted(t, cmd))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != script {
		t.Errorf("round trip mismatch: got %q", raw)
	}
}

// The base64 payload must be inert for the shell that carries it.
func TestScriptCommandPayloadIsShellInert(t *testing.T) {
	cmd, err := Command(PowerShell, []byte("Write-Output 'x'"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range extractQuoted(t, cmd) {
		ok := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '+' || r == '/' || r == '='
		if !ok {
			t.Fatalf("payload contains shell-significant rune %q", r)
		}
	}
}

func TestScriptCommandRejectsOversize(t *testing.T) {
	_, err := Command(PowerShell, []byte(strings.Repeat("Write-Output 'padding'\n", 2000)))
	if err == nil {
		t.Fatal("want an error for an oversize script")
	}
	if !strings.Contains(err.Error(), "wanctl push") {
		t.Errorf("the error should point at the push+run fallback, got: %v", err)
	}
}

func TestNestedPowerShellExpansion(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		// The exact shape that cost an hour: nested invocation, $ inside "".
		{`powershell -NoProfile -Command "Get-Thing | ? {$_.Up}"`, true},
		{`powershell.exe -c "echo $env:TEMP"`, true},
		// No nesting: this is just a normal command, the $ is legitimately ours.
		{`Get-Process | Where-Object { $_.Name -eq 'ssh' }`, false},
		{`echo $env:TEMP`, false},
		// Nested but single-quoted: the outer shell leaves it alone.
		{`powershell -Command 'Get-Thing | ? {$_.Up}'`, false},
		// Nested, double-quoted, no variables: nothing to lose.
		{`powershell -Command "Get-Date"`, false},
	}
	for _, c := range cases {
		if got := NestedPowerShellExpansion(c.cmd); got != c.want {
			t.Errorf("%s: got %v want %v", c.cmd, got, c.want)
		}
	}
}

func TestBomlessNonASCIIPowerShell(t *testing.T) {
	zh := []byte("# 说明\nWrite-Output 'ok'")
	bom := append([]byte{0xEF, 0xBB, 0xBF}, zh...)
	cases := []struct {
		name string
		path string
		data []byte
		want bool
	}{
		{"bomless non-ascii ps1", `C:\infra\x.ps1`, zh, true},
		{"bom present", `C:\infra\x.ps1`, bom, false},
		{"ascii only", `C:\infra\x.ps1`, []byte("Write-Output 'ok'"), false},
		{"not a ps1", `C:\infra\x.txt`, zh, false},
	}
	for _, c := range cases {
		if got := BomlessNonASCIIPowerShell(c.path, c.data); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// extractQuoted pulls the single-quoted base64 payload out of a rendered command.
func extractQuoted(t *testing.T, cmd string) string {
	t.Helper()
	i := strings.Index(cmd, "'")
	j := strings.LastIndex(cmd, "'")
	if i < 0 || j <= i {
		t.Fatalf("no quoted payload in %q", cmd)
	}
	return cmd[i+1 : j]
}

func decodeUTF16LE(b []byte) string {
	units := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		units = append(units, uint16(b[i])|uint16(b[i+1])<<8)
	}
	return string(utf16.Decode(units))
}
