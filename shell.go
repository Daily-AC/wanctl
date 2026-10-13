package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/peterh/liner"

	"wanctl/internal/client"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
)

// `wanctl shell DEVICE` is exec for a person at a keyboard: a prompt that
// sends each line to one remote shell, so cd and variables carry from line to
// line the way they do over ssh. It is line mode, not a terminal: the device
// runs every line without one, so full-screen programs and password prompts
// do not work.
//
// The shell is a workspace (docs/adr/0012), not the legacy per-controller
// session: that one is shared with every other exec from this identity,
// including an AI driving the same device, and a Ctrl-C here would reset it
// under them. A workspace also answers over one reused connection, which is
// what makes a prompt feel like a prompt — measured 2026-10-13 against
// devices on the public relay, 0.25–0.6 s a line against 1.1–1.7 s for a
// legacy exec that dials each time.

// shellRoots are tried in order as the directory the shell is opened on; it
// moves home straight after. A Windows device refuses "/" as not absolute.
var shellRoots = []string{"/", `C:\`}

// The device's per-workspace limits (internal/agent/workspace.go). A shell
// about to cross one is replaced before the next line instead of failing it.
// Output is replaced at half its cap so a single command still has room.
const (
	shellMaxCommands = 128
	shellMaxInput    = 1 << 20
	shellMaxOutput   = 8 << 20
)

func cmdShell(ctx context.Context, args []string) error {
	fs := withHelp(flag.NewFlagSet("shell", flag.ExitOnError))
	target := fs.String("target", "", "device ID or unique name (NS/DEV or DEV)")
	rest := parseAroundPositionals(fs, args)
	if *target == "" && len(rest) == 1 {
		*target, rest = rest[0], nil
	}
	if *target == "" || len(rest) != 0 {
		return fmt.Errorf("usage: wanctl shell <device>")
	}
	if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return fmt.Errorf("wanctl shell needs a terminal; to run commands from a script, use wanctl exec")
	}
	c, err := client.New()
	if err != nil {
		return err
	}
	st, err := c.Status(ctx, *target)
	if err != nil {
		return fmt.Errorf("remote device %q: %w", *target, err)
	}
	if err := shellNeedsBypass(*target, st, settingValue("portal")); err != nil {
		return err
	}
	c.UseWorkspaceLink(client.NewWorkspaceLink())
	s := &remoteShell{c: c, target: *target, name: shellName(*target, st.Name)}
	if err := s.start(ctx, ""); err != nil {
		return err
	}
	return s.loop(ctx)
}

// shellName is what the prompt calls the device: the name the user typed,
// unless that was a bare device ID, which says less than the device's own name.
func shellName(target, deviceName string) string {
	short := target[strings.LastIndex(target, "/")+1:]
	if _, err := uuid.Parse(short); err == nil && deviceName != "" {
		return deviceName
	}
	return short
}

// shellNeedsBypass refuses a device that would stop at every line. In normal
// mode a command no rule covers waits for its owner's approval, and a prompt
// that hangs on each ls is worse than none.
func shellNeedsBypass(target string, st client.AgentStatus, portal string) error {
	if !st.Detailed {
		return fmt.Errorf("%s runs an agent too old to report its policy mode; update it, then retry", target)
	}
	if st.Mode == policy.ModeBypass {
		return nil
	}
	where := "on its page in the portal"
	if portal != "" {
		where = "on its page in the portal (" + portal + ")"
	}
	return fmt.Errorf("%s is in %s mode, and wanctl shell only works on a device in bypass mode,\n"+
		"where commands run without waiting for the owner's approval.\n"+
		"Switch the device to bypass %s and run wanctl shell again,\n"+
		"or keep it in %s mode and use wanctl exec", target, st.Mode, where, st.Mode)
}

type remoteShell struct {
	c      *client.Client
	target string // as the user named it
	name   string // what the prompt calls the device (shellName)
	ref    client.WorkspaceRef
	cwd    string // where the device last said the shell is; "" if it never said
	home   string
	stale  bool // the workspace is gone and the next line needs a new one

	commands int
	input    int
	output   int64

	mu        sync.Mutex
	interrupt context.CancelFunc // stops waiting for the running line; nil at the prompt
}

// start opens a workspace in dir, or when that is empty or gone, on a root
// and then home.
func (s *remoteShell) start(ctx context.Context, dir string) error {
	ref, err := s.c.PrepareWorkspace(ctx, s.target)
	if err != nil {
		return err
	}
	roots := shellRoots
	if dir != "" {
		roots = append([]string{dir}, shellRoots...)
	}
	var errs []error
	for _, root := range roots {
		if _, err := s.c.Workspace(ctx, ref, "open", protocol.Message{Path: root}); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", root, err))
			continue
		}
		s.ref, s.stale = ref, false
		s.commands, s.input, s.output = 0, 0, 0
		if root == dir {
			return nil
		}
		return s.goHome(ctx)
	}
	return fmt.Errorf("could not open a shell on %s: %w", s.target, errors.Join(errs...))
}

func (s *remoteShell) goHome(ctx context.Context) error {
	res, err := s.exec(ctx, client.NewRequestID(), "cd ~", io.Discard)
	if err != nil {
		return err
	}
	if s.home == "" && res.Code == 0 && res.Error == "" {
		s.home = res.Cwd
	}
	return nil
}

// exec runs one line in the workspace and records what the device reports.
func (s *remoteShell) exec(ctx context.Context, rid, line string, out io.Writer) (*protocol.WorkspaceResult, error) {
	s.commands++
	s.input += len(line)
	res, err := s.c.Workspace(ctx, s.ref, "exec", protocol.Message{Command: line, RequestID: rid, WaitMillis: 250})
	if err == nil {
		res, err = awaitWorkspace(ctx, s.c, s.ref, rid, res, out)
	}
	if err != nil {
		return nil, err
	}
	s.output += res.RetainedBytes
	if res.Cwd != "" {
		s.cwd = res.Cwd
	}
	return res, nil
}

func (s *remoteShell) loop(ctx context.Context) error {
	line := liner.NewLiner()
	defer line.Close()
	line.SetCtrlCAborts(true)
	// At the prompt Ctrl-C is a key liner reads. While a line runs the
	// terminal turns it into SIGINT, which cancels that line. A terminal that
	// closes sends SIGHUP; that and SIGTERM end the session, closing the
	// device's shell rather than leaving it open until the device restarts.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	go func() {
		for sig := range sigs {
			if sig == os.Interrupt {
				s.interruptLine()
				continue
			}
			line.Close()
			s.close()
			code := 1
			if n, ok := sig.(syscall.Signal); ok {
				code = 128 + int(n)
			}
			os.Exit(code)
		}
	}()
	for {
		text, err := line.Prompt(s.prompt())
		switch {
		case errors.Is(err, liner.ErrPromptAborted):
			continue
		case errors.Is(err, io.EOF):
			fmt.Println()
			s.close()
			return nil
		case err != nil:
			s.close()
			return err
		}
		cmd := strings.TrimSpace(text)
		if cmd == "" {
			continue
		}
		line.AppendHistory(text)
		if isShellExit(cmd) {
			s.close()
			return nil
		}
		if err := s.run(ctx, cmd); err != nil {
			fmt.Fprintln(os.Stderr, "wanctl: "+errorText(err))
		}
	}
}

// run sends one line, replacing the shell first if it is gone or full.
func (s *remoteShell) run(ctx context.Context, line string) error {
	if why := s.replaceReason(line); why != "" {
		if err := s.restart(ctx, why); err != nil {
			return err
		}
	}
	lineCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.interrupt = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.interrupt = nil
		s.mu.Unlock()
		cancel()
	}()

	term, flush := deviceOutput(os.Stdout)
	out := &shellOutput{w: term}
	rid := client.NewRequestID()
	res, err := s.exec(lineCtx, rid, line, out)
	out.end()
	flush()
	if err != nil && lineCtx.Err() != nil && ctx.Err() == nil {
		return s.cancelLine(ctx, rid)
	}
	if err != nil {
		return s.checkAlive(ctx, err)
	}
	if res.Truncated {
		fmt.Fprintln(os.Stderr, "wanctl: output truncated; the device keeps at most 8 MiB of output per shell")
	}
	if res.Error != "" {
		return s.checkAlive(ctx, errors.New(res.Error))
	}
	return nil
}

func (s *remoteShell) replaceReason(line string) string {
	switch {
	case s.stale:
		return "the previous shell on the device is gone"
	case s.commands >= shellMaxCommands,
		s.input+len(line) > shellMaxInput,
		s.output >= shellMaxOutput/2:
		return "this shell reached the device's limit of 128 commands or 8 MiB of output"
	}
	return ""
}

func (s *remoteShell) interruptLine() {
	s.mu.Lock()
	cancel := s.interrupt
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// cancelLine stops the line on the device. Stopping a command reliably takes
// its shell down with it (docs/adr/0011), so a new one replaces it unless the
// line had already finished.
func (s *remoteShell) cancelLine(ctx context.Context, requestID string) error {
	fmt.Fprintln(os.Stderr, "^C")
	if _, err := s.c.Workspace(ctx, s.ref, "cancel", protocol.Message{RequestID: requestID}); err != nil {
		if st, serr := s.c.Workspace(ctx, s.ref, "status", protocol.Message{}); serr == nil && st.State == "open" {
			return nil
		}
	}
	return s.restart(ctx, "stopping a command ends the device's shell")
}

// checkAlive reports a failed line, and marks the shell for replacement when
// the failure took the device's shell with it.
func (s *remoteShell) checkAlive(ctx context.Context, lineErr error) error {
	if st, err := s.c.Workspace(ctx, s.ref, "status", protocol.Message{}); err == nil && st.State != "open" {
		s.stale = true
	}
	return lineErr
}

func (s *remoteShell) restart(ctx context.Context, why string) error {
	s.closeWorkspace(ctx)
	if err := s.start(ctx, s.cwd); err != nil {
		s.stale = true
		return err
	}
	where := s.displayDir()
	if where == "" {
		// An older agent does not say where its shell is, so the
		// new one could only be opened at home.
		where = "the home directory"
	}
	fmt.Fprintf(os.Stderr, "wanctl: %s; opened a new one in %s (variables and functions set before are gone)\n", why, where)
	return nil
}

func (s *remoteShell) closeWorkspace(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	s.c.Workspace(ctx, s.ref, "close", protocol.Message{})
}

// close ends the session's shell on the device. Close and cancel travel on a
// connection of their own, so this works while a line is still waiting.
func (s *remoteShell) close() { s.closeWorkspace(context.Background()) }

func (s *remoteShell) prompt() string {
	p := s.name
	if dir := s.displayDir(); dir != "" {
		p += ":" + dir
	}
	return promptSafe(p) + "$ "
}

// displayDir is the shell's directory with the home prefix shortened to ~.
func (s *remoteShell) displayDir() string {
	dir, home := s.cwd, s.home
	// A home that is a filesystem root ("/", `C:\`) would turn every path into ~.
	if len(strings.Trim(home, `/\:`)) <= 1 {
		return dir
	}
	if dir == home {
		return "~"
	}
	if strings.HasPrefix(dir, home) && strings.ContainsRune(`/\`, rune(dir[len(home)])) {
		return "~" + dir[len(home):]
	}
	return dir
}

// promptSafe keeps text the device chose (its name, a directory name) from
// carrying control characters into the prompt, which liner refuses outright.
func promptSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.Is(unicode.C, r) {
			return '?'
		}
		return r
	}, s)
}

func isShellExit(line string) bool {
	f := strings.Fields(line)
	return len(f) <= 2 && (f[0] == "exit" || f[0] == "logout")
}

// shellOutput drops the one newline the device adds to every command. Its
// POSIX shell ends a command by printing a marker on a line of its own, and
// the newline before that marker was never the command's; left in, each
// command would be followed by a blank line. PowerShell's marker adds none,
// and its lines end in \r\n, which is how the two are told apart.
type shellOutput struct {
	w    io.Writer
	held bool // a trailing \n not yet known to be the last byte
	last byte // the last byte written through; 0 before any
}

func (o *shellOutput) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if o.held {
		if _, err := o.w.Write([]byte{'\n'}); err != nil {
			return 0, err
		}
		o.held, o.last = false, '\n'
	}
	body := p
	if p[len(p)-1] == '\n' {
		prev := o.last
		if len(p) > 1 {
			prev = p[len(p)-2]
		}
		if prev != '\r' {
			body, o.held = p[:len(p)-1], true
		}
	}
	if len(body) > 0 {
		if _, err := o.w.Write(body); err != nil {
			return 0, err
		}
		o.last = body[len(body)-1]
	}
	return len(p), nil
}

// end discards the held newline and leaves the cursor at the start of a line,
// where the prompt goes.
func (o *shellOutput) end() {
	if o.last != 0 && o.last != '\n' {
		o.w.Write([]byte{'\n'})
	}
}
