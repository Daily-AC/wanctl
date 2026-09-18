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
	"sync/atomic"
	"time"

	"wanctl/internal/protocol"
)

// DefaultShell returns the interpreter used for a session on this OS.
func DefaultShell() string {
	switch runtime.GOOS {
	case "windows":
		return "powershell"
	case "android":
		return androidShell(func(path string) bool {
			_, err := os.Stat(path)
			return err == nil
		})
	}
	return "/bin/sh"
}

// androidShell finds an interpreter on Android, where /bin/sh is not a given.
//
//   - /system/bin/sh (mksh) exists on every Android build and is the only shell
//     the agent can actually start. See below.
//   - /bin is a symlink to /system/bin from Android 11 on, so /bin/sh resolves
//     there too; older devices have no /bin at all, which is why it cannot be
//     the default.
//
// Termux's own $PREFIX/bin/sh is deliberately *not* preferred, reversing the
// first attempt at this. The agent cannot exec it: Android forbids the
// untrusted_app domain from exec'ing files in an app's private data directory,
// and Termux only gets away with it because libtermux-exec.so — preloaded into
// its shell — rewrites every execve to go through the dynamic linker. A
// CGO-free Go binary loads no such library, so the syscall is refused. Measured
// on a vivo PA2353 running Termux, 2026-08-06:
//
//	exec($PREFIX/bin/sh)      → fork/exec …/usr/bin/sh: permission denied
//	exec($PREFIX/bin/bash)    → permission denied
//	exec(/system/bin/sh)      → ok
//
// The original reason for preferring the Termux shell — "otherwise the session
// has none of the user's tools" — turned out to be false. A session in
// /system/bin/sh inherits Termux's PATH, and Termux binaries launched *by that
// shell* run fine, because mksh does the exec, not us:
//
//	/system/bin/sh -c "git --version"  → git version 2.52.0   (Termux's git)
//
// So the system shell costs nothing and is the only one that starts.
func androidShell(exists func(string) bool) string {
	for _, c := range []string{"/system/bin/sh", "/bin/sh"} {
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

// winExitEpilogue makes a one-shot's process exit code mean what the caller
// expects. `powershell -Command <source>` exits 0 or 1 from $? regardless of
// what any native program inside returned, so `wanctl exec -oneshot 'cmd /c
// exit 7'` reported 1 while the same command in session mode reported 7 — the
// session path reads $LASTEXITCODE in its end-of-command marker and never had
// the bug. Ending the source with the same expression closes the gap, so exit
// codes mean one thing in both modes.
//
// The leading newline terminates whatever the command ended with (a comment,
// say) so the epilogue is always its own statement. Commands that call `exit`
// themselves never reach it, which is the desired precedence.
const winExitEpilogue = "\nexit $(if($null -ne $LASTEXITCODE){$LASTEXITCODE}else{[int](-not $?)})"

// ShellSession is a long-lived interpreter process whose working directory and
// environment persist across commands. Each command is delimited by a unique
// sentinel that the shell echoes after the command, carrying its exit code, so
// the agent knows exactly when a command's output has ended.
//
// stderr is merged into stdout (a single ordered stream) for simplicity; the
// controller receives it all as stdout.
type ShellSession struct {
	shell string
	cmd   *exec.Cmd
	stdin io.WriteCloser
	// reader is the read half of the output pipe, kept so cancellation can
	// close it directly. See endOutput.
	reader *io.PipeReader
	out    *bufio.Reader
	token  string
	mu     sync.Mutex // serializes commands on this session
	// closed is atomic because Closed() is called by the agent while it holds
	// the lock that guards every session on the device. Reading it must never
	// wait for the command running in this one.
	closed atomic.Bool

	// container holds the shell and everything it starts, so cancelling a
	// command is one kernel operation on a named unit rather than a guess at
	// which processes belong to it. See session_container.go.
	container    *sessionContainer
	containerOff sync.Once
	// killContainer is the container kill cancelNow performs. It is a field so
	// a test can make the kill fail without also replacing what cancelNow does
	// around it, which is the part worth testing.
	killContainer func() error
	// gate binds a cancellation to the single request that armed it.
	gate *cancelGate
}

// endOutput makes every pending and future read of the session's output fail at
// once.
//
// Cancellation must not depend on the shell's descendants. The shell's stdout is
// an OS pipe inherited by everything it forks, so a process that left the
// container — `set -m; sleep 600 & wait` moves the sleep into its own process
// group, and setsid() leaves outright — still holds the write end after the
// container is killed. The copier that feeds this reader then never sees EOF,
// and a Read waiting for the end-of-command marker would wait forever: the
// command would never return, Closed() would never answer, and the session
// could never be replaced.
//
// Closing the reader here cuts that dependency. The escaped process keeps
// running — that is its documented privilege — but it no longer holds the
// session hostage.
func (s *ShellSession) endOutput(cause error) {
	if s.reader != nil {
		s.reader.CloseWithError(cause)
	}
}

// cancelNow is what a cancellation does: end the session's processes and stop
// waiting on their output. The two are separate steps on purpose. Killing the
// container does not guarantee the output pipe closes, because a descendant
// that escaped the container still holds it, so the reader is cut here rather
// than left to a copier that may never see EOF. That is also what makes a kill
// that *failed* reach the caller immediately instead of when the command it
// could not stop happens to end.
func (s *ShellSession) cancelNow() error {
	s.closed.Store(true)
	err := s.killContainer()
	s.endOutput(ErrSessionCancelled)
	return err
}

// releaseContainer ends the session's processes and gives back whatever the
// container holds. Idempotent: a cancel kills the container and Close is still
// expected afterwards, on Windows to release the job handle.
func (s *ShellSession) releaseContainer() {
	s.containerOff.Do(func() {
		if s.container == nil {
			return
		}
		s.container.Kill()
		s.container.Close()
	})
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
	hideConsole(cmd)
	prepareSessionContainer(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw // merge stderr into the single stream
	// WaitDelay bounds the reaper below. A descendant that left the container
	// still holds the shell's stdout, so without it cmd.Wait blocks forever on
	// a copier that will never see EOF, leaking that goroutine and the pipe's
	// descriptors for the life of the process.
	cmd.WaitDelay = sessionWaitDelay
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	// One cleanup path for every failure after Start succeeded: the process is
	// killed and reaped, and both halves of the output pipe are closed, so no
	// copier is left blocked writing into a pipe nobody reads.
	abandon := func(cause error) (*ShellSession, error) {
		cmd.Process.Kill()
		stdin.Close()
		pw.Close()
		pr.Close()
		cmd.Wait()
		return nil, cause
	}

	container, err := captureSession(cmd)
	if err != nil {
		return abandon(err)
	}
	go func() {
		// cmd.Wait waits for I/O after the kernel has already reaped the
		// process. The pid — and the Unix process-group id that is the same
		// number — can be reused in that window (issue #110). Kill leftover
		// group members first (issue #111: a background process that never
		// called setsid still holds the pgid after a natural `exit`), then
		// drop the identity at the kernel wait so a later Kill cannot hit a
		// stranger. Let Wait finish copying after that.
		if cmd.Process != nil {
			_, _ = cmd.Process.Wait()
		}
		_ = container.Kill()
		container.reap()
		_ = cmd.Wait()
		pw.Close()
	}()

	s.container = container
	s.cmd = cmd
	s.stdin = stdin
	s.reader = pr
	s.out = bufio.NewReader(pr)
	s.killContainer = container.Kill
	s.gate = newCancelGate(s.cancelNow)

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

// captureSession is captureSessionContainer behind a variable, so a test can
// fail the containment step and check that a shell which cannot be contained is
// cleaned up rather than left running with a blocked output copier.
var captureSession = captureSessionContainer

// sessionWaitDelay is how long the reaper gives output already in flight after
// the shell exits before it closes the pipes out from under it. It matches the
// one-shot cancel hooks.
const sessionWaitDelay = 2 * time.Second

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
	return s.ExecInDirContext(context.Background(), command, cwd, out)
}

// ExecInDirContext is ExecInDir with cancellation. When ctx is done the whole
// session is destroyed: the shell and everything it started die together, and
// the caller must build a fresh session for the next command, which therefore
// starts in the default working directory with a default environment. That
// reset is the price of a cancel that actually stops everything the command set
// in motion — see session_container.go and
// docs/adr/0011-session-cancel-resets-session.md for why keeping the shell
// cannot be made correct.
//
// A cancelled command reports an error rather than the exit status the shell
// may have printed, which is what the one-shot path does too: the caller must
// be able to tell "the controller stopped this" from "the command itself
// failed". A kill that failed is reported as such and never swallowed, because
// the caller would otherwise be told the command stopped when it did not.
func (s *ShellSession) ExecInDirContext(ctx context.Context, command, cwd string, out io.Writer) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A request that was already cancelled — the controller left while it sat
	// behind another command on this session — must not run at all.
	if err := ctx.Err(); err != nil {
		return -1, err
	}
	// Another request may have cancelled and dropped this session between the
	// caller acquiring it and reaching here. Nothing has been submitted, so say
	// so precisely: this is the one failure the caller may retry on a fresh
	// session.
	if s.closed.Load() {
		return -1, ErrSessionUnusable
	}
	disarm := s.gate.arm(ctx)
	code, err := s.runLocked(command, cwd, out)
	// Disarming happens under the session lock and waits for a cancellation
	// that is already running, so no cancel can survive into the next request.
	if fired, killErr := disarm(); fired {
		s.closed.Store(true)
		if killErr != nil {
			return -1, fmt.Errorf("%w, but the device could not stop it: %v", ErrSessionCancelled, killErr)
		}
		return -1, ErrSessionCancelled
	}
	return code, err
}

func (s *ShellSession) runLocked(command, cwd string, out io.Writer) (int, error) {
	if cwd != "" {
		code, err := s.changeDirLocked(cwd, out)
		if err != nil || code != 0 {
			return code, err
		}
	}
	return s.execLocked(command, out)
}

func (s *ShellSession) execLocked(command string, out io.Writer) (int, error) {
	if s.closed.Load() {
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
			s.closed.Store(true)
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

// Closed reports whether the session has been torn down. It takes no lock: the
// agent asks this while holding the lock that guards every session on the
// device, so waiting here for one session's command would stall all of them.
func (s *ShellSession) Closed() bool { return s.closed.Load() }

// Close terminates the session: the shell and everything still running inside
// its container. Killing only the shell would leave its children orphaned and
// still holding the device's resources.
func (s *ShellSession) Close() {
	// Mark, kill and cut the output before taking the lock. A command still
	// waiting on the shell holds that lock, and these three things are exactly
	// what end its wait — locking first would make Close hang on the session it
	// is trying to tear down.
	first := !s.closed.Swap(true)
	s.releaseContainer()
	if s.container == nil && s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Kill()
	}
	s.endOutput(fmt.Errorf("session closed"))
	s.mu.Lock()
	defer s.mu.Unlock()
	if first && s.stdin != nil {
		s.stdin.Close()
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
		cmd = exec.CommandContext(ctx, shell, "-NoProfile", "-NoLogo", "-NonInteractive", "-Command", winUTF8Prologue+command+winExitEpilogue)
	} else {
		cmd = exec.CommandContext(ctx, shell, "-c", command)
	}
	configureCommandCancellation(cmd)
	hideConsole(cmd)
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
