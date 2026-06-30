// Package server holds the device-side request handlers forked from lanctl: a
// persistent shell session (working dir + env survive across commands), one-shot
// command execution, and file upload/download. The wanctl agent drives these;
// the lanctl TLS-listener/mDNS lifecycle is replaced by the agent control loop.
package server

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"wanctl/internal/protocol"
)

// DefaultShell returns the interpreter used for a session on this OS.
func DefaultShell() string {
	if runtime.GOOS == "windows" {
		return "powershell"
	}
	return "/bin/sh"
}

// winUTF8Prologue forces a PowerShell session to emit UTF-8 so native-tool
// output isn't mangled. Without it, native Windows programs emit UTF-16LE and
// PowerShell decodes them with the OEM code page, leaving the zero high-byte of
// every character as a separator — the infamous "T h e   W i n d o w s" output.
//
//   - [Console]::OutputEncoding governs how PowerShell decodes a child process's
//     stdout; setting it to UTF-8 fixes code-page-respecting tools (netsh, etc.).
//   - $OutputEncoding governs the bytes PowerShell sends when piping INTO a
//     native command.
//   - WSL_UTF8=1 is the only thing that fixes wsl.exe, which ignores the console
//     code page and always emits UTF-16LE otherwise (`wsl --status/--version`).
//
// Wrapped in try/catch because the encoding setters can throw when stdout is a
// redirected pipe rather than a console; if they do, WSL_UTF8 still applies and
// covers the most-cited case. All statements are assignments → no stdout, so it
// is safe to prepend to a command or run as a session prologue.
const winUTF8Prologue = `try{$e=New-Object System.Text.UTF8Encoding $false;[Console]::OutputEncoding=$e;$OutputEncoding=$e}catch{};$env:WSL_UTF8='1';`

// ShellSession is a long-lived interpreter process whose working directory and
// environment persist across commands. Each command is delimited by a unique
// sentinel that the shell echoes after the command, carrying its exit code, so
// the agent knows exactly when a command's output has ended.
//
// stderr is merged into stdout (a single ordered stream) for simplicity; the
// controller receives it all as stdout.
type ShellSession struct {
	shell  string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    *bufio.Reader
	token  string
	mu     sync.Mutex // serializes commands on this session
	closed bool
}

// NewShellSession starts a persistent shell process.
func NewShellSession(shell string) (*ShellSession, error) {
	if shell == "" {
		shell = DefaultShell()
	}
	tok := make([]byte, 8)
	if _, err := rand.Read(tok); err != nil {
		return nil, err
	}
	s := &ShellSession{shell: shell, token: hex.EncodeToString(tok)}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command(shell, "-NoProfile", "-NoLogo", "-NonInteractive", "-Command", "-")
	} else {
		cmd = exec.Command(shell, "-s")
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw // merge stderr into the single stream
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() { cmd.Wait(); pw.Close() }()

	s.cmd = cmd
	s.stdin = stdin
	s.out = bufio.NewReader(pr)

	// On Windows, force UTF-8 output once for the life of the session so native
	// tools (notably wsl.exe) aren't returned with a space between every char.
	// The prologue is pure assignments → no stdout → it can't desync the marker
	// protocol of the first Exec call.
	if runtime.GOOS == "windows" {
		if _, err := io.WriteString(s.stdin, winUTF8Prologue+"\n"); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

// markerPrefix returns the sentinel line the shell prints after each command.
func (s *ShellSession) markerPrefix() string { return "<<<WANCTL_END:" }

// writeCommand sends the user command followed by the sentinel echo.
func (s *ShellSession) writeCommand(command string) error {
	var b strings.Builder
	b.WriteString(command)
	b.WriteString("\n")
	if runtime.GOOS == "windows" {
		// $LASTEXITCODE is only set by native executables; fall back to $? for
		// cmdlet success/failure.
		b.WriteString(fmt.Sprintf(
			"\"%s$(if($null -ne $LASTEXITCODE){$LASTEXITCODE}else{[int](-not $?)}):%s>>>\"\n",
			s.markerPrefix(), s.token))
	} else {
		b.WriteString(fmt.Sprintf("printf '\\n%s%%s:%s>>>\\n' \"$?\"\n", s.markerPrefix(), s.token))
	}
	_, err := io.WriteString(s.stdin, b.String())
	return err
}

// Exec runs command, streaming output to out, and returns the exit code.
func (s *ShellSession) Exec(command string, out io.Writer) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return -1, fmt.Errorf("session closed")
	}
	if err := s.writeCommand(command); err != nil {
		return -1, err
	}
	suffix := ":" + s.token + ">>>"
	for {
		line, err := s.out.ReadString('\n')
		if line != "" {
			if i := strings.Index(line, s.markerPrefix()); i >= 0 && strings.Contains(line[i:], suffix) {
				// Anything before the marker on this line is real output.
				if i > 0 {
					out.Write([]byte(line[:i]))
				}
				rest := line[i+len(s.markerPrefix()):]
				code := rest[:strings.Index(rest, ":")]
				n, _ := strconv.Atoi(strings.TrimSpace(code))
				return n, nil
			}
			out.Write([]byte(line))
		}
		if err != nil {
			s.closed = true
			return -1, fmt.Errorf("shell stream ended: %w", err)
		}
	}
}

// Closed reports whether the session has been torn down.
func (s *ShellSession) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Close terminates the shell process.
func (s *ShellSession) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.stdin.Close()
	if s.cmd.Process != nil {
		s.cmd.Process.Kill()
	}
}

// RunOneShot executes a command in a fresh shell with no persistent state and
// streams merged output to out.
func RunOneShot(shell, command string, out io.Writer) (int, error) {
	if shell == "" {
		shell = DefaultShell()
	}
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command(shell, "-NoProfile", "-NoLogo", "-NonInteractive", "-Command", winUTF8Prologue+command)
	} else {
		cmd = exec.Command(shell, "-c", command)
	}
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return -1, err
}

// FrameWriter adapts a writer into framed output of a fixed type, so command
// output can be streamed to the controller as protocol frames.
func FrameWriter(w io.Writer, t protocol.FrameType) io.Writer {
	return frameWriter{w: w, t: t}
}

type frameWriter struct {
	w io.Writer
	t protocol.FrameType
}

func (f frameWriter) Write(p []byte) (int, error) {
	if err := protocol.WriteFrame(f.w, f.t, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
