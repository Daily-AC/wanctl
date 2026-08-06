package script

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Running a script on a remote device used to mean composing one shell command
// string and hoping it survived the trip. It usually didn't, and the two ways it
// failed both looked like the remote machine's fault rather than a quoting
// problem:
//
//  1. `wanctl exec` hands the command to the device's shell as *source*. On
//     Windows that shell is PowerShell, so a command like
//     `powershell -Command "... $_.Name ..."` is parsed twice — the outer
//     PowerShell expands `$_` and `$null` to nothing before the inner one ever
//     sees them. The script then dies with
//     `The term '.Name' is not recognized`, which reads like a PowerShell bug.
//  2. Pushing a .ps1 and running it with -File dodges that, but Windows
//     PowerShell 5.1 reads a BOM-less file as the ANSI code page. Any non-ASCII
//     text comes back mojibake; if it sits inside a "double-quoted string" the
//     mangled bytes eat the closing quote and PowerShell starts echoing the
//     script instead of running it.
//
// Command removes both by never letting the script be shell source and
// never letting the interpreter guess an encoding: the bytes are base64'd (and
// for PowerShell, transcoded to the UTF-16LE that -EncodedCommand mandates), so
// what crosses the wire is `[A-Za-z0-9+/=]` — inert under every shell we target.

// maxCommand caps the generated command string. Windows command lines top
// out near 32767 characters and -oneshot builds a real command line, so leave
// room for the interpreter invocation and the UTF-8 prologue the agent prepends.
const maxCommand = 24000

// psPrologue silences PowerShell's progress records for the duration of
// the script. A child PowerShell whose stderr is a pipe rather than a console
// serialises progress records as CLIXML, and cmdlets like Get-NetIPConfiguration
// emit dozens of them — hundreds of lines of `<Obj S="progress">` burying the
// script's actual output. Progress bars have no meaning down a relayed pipe, so
// dropping them costs nothing and is what makes the output readable.
//
// It is deliberately only ProgressPreference: ErrorActionPreference and the
// error stream are the script author's business, and a tool that quietly changed
// them would hide real failures.
const psPrologue = "$ProgressPreference='SilentlyContinue';\n"

// Interp is the interpreter a script is handed to on the device.
type Interp string

const (
	PowerShell Interp = "powershell"
	POSIX      Interp = "sh"
)

// ForPath picks an interpreter from the file extension. The extension is
// the whole contract: it is visible in the command the user typed, so there is
// no hidden probe of the remote OS to disagree with later.
func ForPath(path string) (Interp, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ps1":
		return PowerShell, nil
	case ".sh", ".bash", "":
		return POSIX, nil
	default:
		return "", fmt.Errorf("cannot infer interpreter from %q; name it .ps1 or .sh, or state it explicitly (powershell|sh)", filepath.Base(path))
	}
}

// ParseInterp validates an explicit -interp value.
func ParseInterp(s string) (Interp, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case string(PowerShell), "pwsh", "ps1":
		return PowerShell, nil
	case string(POSIX), "bash", "posix":
		return POSIX, nil
	default:
		return "", fmt.Errorf("unknown interpreter %q (want powershell or sh)", s)
	}
}

// Command renders script bytes into a command string that the device's
// shell can carry without interpreting any of the script's own syntax.
func Command(interp Interp, script []byte) (string, error) {
	var cmd string
	switch interp {
	case PowerShell:
		// -EncodedCommand takes base64 of UTF-16LE. Feeding it that directly
		// means the script's encoding is stated rather than guessed, which is
		// the whole BOM class of bug gone: strip a UTF-8 BOM if present (it
		// would otherwise survive as a stray U+FEFF token) and transcode.
		src := bytes.TrimPrefix(script, []byte{0xEF, 0xBB, 0xBF})
		if !utf8.Valid(src) {
			return "", fmt.Errorf("script is not valid UTF-8; convert it before sending (PowerShell needs UTF-16LE, which wanctl derives from UTF-8)")
		}
		enc := base64.StdEncoding.EncodeToString(utf16LE(psPrologue + string(src)))
		// Single quotes so the outer shell treats the blob as a literal even
		// though base64 can contain '+', '/' and '='.
		cmd = "powershell -NoProfile -NonInteractive -EncodedCommand '" + enc + "'"
	case POSIX:
		enc := base64.StdEncoding.EncodeToString(script)
		cmd = "printf %s '" + enc + "' | base64 -d | /bin/sh"
	default:
		return "", fmt.Errorf("unknown interpreter %q", interp)
	}
	if len(cmd) > maxCommand {
		return "", fmt.Errorf("script too large to inline (%d bytes of command, limit %d); push it and run it instead:\n"+
			"  wanctl push -target <dev> <local> '<remote>'\n"+
			"  wanctl exec -target <dev> '<interpreter> <remote>'", len(cmd), maxCommand)
	}
	return cmd, nil
}

// utf16LE encodes s as UTF-16 little-endian, the encoding PowerShell's
// -EncodedCommand expects.
func utf16LE(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 0, len(units)*2)
	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

// NestedPowerShellExpansion reports whether command looks like the double-parse
// footgun: a nested `powershell -Command "..."` whose double-quoted argument
// contains a `$`. The outer shell expands those before the inner interpreter
// runs, so the inner script silently loses its variables.
//
// Deliberately narrow — it only fires on a nested invocation, so an ordinary
// one-level command using $env: or $PSVersionTable is never flagged.
var nestedPSCommand = regexp.MustCompile(`(?i)\bpowershell(\.exe)?\b[^|;]*?\s-(c|command)\b`)

func NestedPowerShellExpansion(command string) bool {
	loc := nestedPSCommand.FindStringIndex(command)
	if loc == nil {
		return false
	}
	rest := command[loc[1]:]
	open := strings.Index(rest, `"`)
	if open < 0 {
		return false
	}
	close := strings.Index(rest[open+1:], `"`)
	if close < 0 {
		// Unterminated from our position; scan what is there.
		return strings.Contains(rest[open+1:], "$")
	}
	return strings.Contains(rest[open+1:open+1+close], "$")
}

// BomlessNonASCIIPowerShell reports whether data is a PowerShell script that
// Windows PowerShell 5.1 will misread: non-ASCII bytes with no UTF-8 BOM. 5.1
// falls back to the ANSI code page for BOM-less files, so the text is mangled —
// harmless in comments, fatal inside a "double-quoted string" where the mangled
// bytes can swallow the closing quote.
func BomlessNonASCIIPowerShell(remotePath string, data []byte) bool {
	if !strings.EqualFold(filepath.Ext(remotePath), ".ps1") {
		return false
	}
	if bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}) {
		return false
	}
	for _, b := range data {
		if b >= 0x80 {
			return true
		}
	}
	return false
}
