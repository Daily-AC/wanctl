// Package server holds the device-side request handlers forked from lanctl: a
// persistent shell session (working dir + env survive across commands), one-shot
// command execution, and file upload/download. The wanctl agent drives these;
// the lanctl TLS-listener/mDNS lifecycle is replaced by the agent control loop.
package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"wanctl/internal/protocol"
)

// DefaultShell returns the interpreter used for a session on this OS.
func DefaultShell() string {
	switch runtime.GOOS {
	case "windows":
		return "powershell"
	case "android":
		return androidShell(os.Getenv, func(path string) bool {
			_, err := os.Stat(path)
			return err == nil
		})
	}
	return "/bin/sh"
}

// androidShell finds an interpreter on Android, where /bin/sh is not a given.
//
// Three environments, three answers:
//
//   - Termux — $PREFIX/bin/sh is the shell the user actually has tools in.
//     Picking the system shell there would hand the controller a session with
//     none of the Termux PATH, which looks like a broken device rather than a
//     deliberate choice.
//   - Any Android — /system/bin/sh always exists (mksh).
//   - Android 11+ — /bin is a symlink to /system/bin, so /bin/sh resolves too.
//     Older devices have no /bin at all, which is why it cannot be the default.
//
// The order is preference, not availability: the last entry is a shell that is
// present on every Android release, so the fallback is never unreachable.
func androidShell(getenv func(string) string, exists func(string) bool) string {
	var candidates []string
	if prefix := strings.TrimSpace(getenv("PREFIX")); prefix != "" {
		candidates = append(candidates, prefix+"/bin/sh")
	}
	candidates = append(candidates, "/system/bin/sh", "/bin/sh")
	for _, c := range candidates {
		if exists(c) {
			return c
		}
	}
	return "/system/bin/sh"
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

// Exec runs command in the session's current directory, streaming output to
// out, and returns the exit code.
func (s *ShellSession) Exec(command string, out io.Writer) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execLocked(command, out)
}

// ExecInDir runs command after changing the persistent session to cwd. The cwd
// value is passed through a data file consumed by a fixed shell protocol; its
// contents are never inserted into shell source. An empty cwd preserves the
// session's current working directory.
func (s *ShellSession) ExecInDir(command, cwd string, out io.Writer) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cwd != "" {
		code, err := s.changeDirLocked(cwd, out)
		if err != nil || code != 0 {
			return code, err
		}
	}
	return s.execLocked(command, out)
}

func (s *ShellSession) execLocked(command string, out io.Writer) (int, error) {
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

func (s *ShellSession) changeDirLocked(cwd string, out io.Writer) (int, error) {
	f, err := os.CreateTemp("", "wanctl-cwd-"+s.token+"-*")
	if err != nil {
		return -1, err
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := io.WriteString(f, cwd); err != nil {
		f.Close()
		return -1, err
	}
	if err := f.Close(); err != nil {
		return -1, err
	}
	return s.execLocked(changeDirCommand(runtime.GOOS, path), out)
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

// RunOneShot executes a command in a fresh shell whose process working
// directory is cwd, streaming merged output to out. Passing cwd through
// exec.Cmd.Dir keeps it entirely outside the shell source.
func RunOneShot(shell, command, cwd string, out io.Writer) (int, error) {
	return RunOneShotContext(context.Background(), shell, command, cwd, out)
}

// RunOneShotContext executes a command in a fresh shell and terminates it when
// ctx is cancelled or reaches its deadline. cwd is passed through exec.Cmd.Dir.
func RunOneShotContext(ctx context.Context, shell, command, cwd string, out io.Writer) (int, error) {
	if shell == "" {
		shell = DefaultShell()
	}
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, shell, "-NoProfile", "-NoLogo", "-NonInteractive", "-Command", winUTF8Prologue+command)
	} else {
		cmd = exec.CommandContext(ctx, shell, "-c", command)
	}
	configureCommandCancellation(cmd)
	cmd.Dir = cwd
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return -1, ctxErr
	}
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return -1, err
}

// changeDirCommand is fixed protocol source for consuming a cwd data file. The
// file path is generated locally; the remote cwd value is only ever file data.
func changeDirCommand(goos, dataPath string) string {
	if goos == "windows" {
		return "$wanctlCwd=[IO.File]::ReadAllText(" + quotePowerShellLiteral(dataPath) + ",[Text.Encoding]::UTF8); Set-Location -LiteralPath $wanctlCwd"
	}
	return "{ IFS= read -r WANCTL_CWD < " + quotePOSIXLiteral(dataPath) + " || [ -n \"$WANCTL_CWD\" ]; } && cd -- \"$WANCTL_CWD\""
}

func quotePOSIXLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func quotePowerShellLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
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
