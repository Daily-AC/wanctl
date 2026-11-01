package elevate

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"

	"wanctl/internal/adb"
)

// Stream separates the three process pipes and owns their lifetime. Closing
// stdin sends EOF; Close aborts; Wait reports remote exit or a transport error.
type Stream interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Stderr() io.ReadCloser
	Wait() (int, error)
	Close() error
}
type streamChannel interface {
	Stream(context.Context, string, string) (Stream, error)
}

func (m *Manager) Stream(ctx context.Context, via Kind, command, cwd string) (Stream, error) {
	c, _, err := m.Select(ctx, via)
	if err != nil {
		return nil, err
	}
	return m.openStream(ctx, c, command, cwd, false)
}

// ShellStream explicitly requests uid 2000, including on a rooted device.
// A selected channel that cannot provide this identity fails without fallback.
func (m *Manager) ShellStream(ctx context.Context, via Kind, command, cwd string) (Stream, error) {
	c, _, err := m.Select(ctx, via)
	if err != nil {
		return nil, err
	}
	return m.openStream(ctx, c, command, cwd, true)
}

// Stream transport failures replace stale availability in the same status
// cache as Run/doctor. Fresh background probes still decide current link state.
func (m *Manager) openStream(ctx context.Context, c Channel, command, cwd string, shell bool) (Stream, error) {
	report := func(err error) {
		if err == nil || ctx.Err() != nil || c.Kind() != KindADB {
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.enabled {
			m.cache[KindADB] = Status{Kind: KindADB, Link: linkOf(err), Reason: err.Error(), CheckedAt: m.now()}
		}
	}
	s, err := openStream(ctx, c, command, cwd, shell)
	if err != nil {
		report(err)
		return nil, err
	}
	if s == nil {
		return nil, fmt.Errorf("elevation channel %q returned no stream", c.Kind())
	}
	tracked := &statusStream{Stream: s, done: make(chan struct{})}
	go func() {
		tracked.code, tracked.err = s.Wait()
		if !tracked.closing.Load() {
			report(tracked.err)
		}
		close(tracked.done)
	}()
	return tracked, nil
}

type statusStream struct {
	Stream
	closing atomic.Bool
	done    chan struct{}
	code    int
	err     error
}

func (s *statusStream) Close() error       { s.closing.Store(true); return s.Stream.Close() }
func (s *statusStream) Wait() (int, error) { <-s.done; return s.code, s.err }

func openStream(ctx context.Context, c Channel, command, cwd string, shell bool) (Stream, error) {
	if shell {
		command = `if [ "$(id -u)" != 2000 ]; then echo 'OpenCU requires the shell user (uid 2000)' >&2; exit 1; fi; ` + command
		if c.Kind() == KindSu {
			if su, ok := c.(interface {
				ShellStream(context.Context, string, string) (Stream, error)
			}); ok {
				return su.ShellStream(ctx, command, cwd)
			}
			return nil, fmt.Errorf("elevation channel %q cannot stream as shell", c.Kind())
		}
	}
	sc, ok := c.(streamChannel)
	if !ok {
		return nil, fmt.Errorf("elevation channel %q does not support streams", c.Kind())
	}
	return sc.Stream(ctx, command, cwd)
}

func (a *ADB) Stream(ctx context.Context, command, cwd string) (Stream, error) {
	// Reuse discovery/authentication and its link-state sentences, never the Run
	// connection or its mutex. No retry after opening a stream.
	dedicated := &ADB{dial: a.dial, ports: a.ports, configDir: a.configDir, name: a.name}
	conn, _, _, err := dedicated.connect(ctx)
	if err != nil {
		return nil, err
	}
	opener, ok := conn.(interface {
		Stream(context.Context, string) (*adb.Stream, error)
	})
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("adb connection does not support shell_v2 streams")
	}
	if cwd != "" {
		command = "cd " + quotePOSIX(cwd) + " && " + command
	}
	return opener.Stream(ctx, command)
}

func (s *Su) Stream(ctx context.Context, command, cwd string) (Stream, error) {
	return s.stream(ctx, command, cwd, false)
}
func (s *Su) ShellStream(ctx context.Context, command, cwd string) (Stream, error) {
	return s.stream(ctx, command, cwd, true)
}
func (s *Su) stream(ctx context.Context, command, cwd string, shellUser bool) (Stream, error) {
	s.mu.Lock()
	form := s.form
	s.mu.Unlock()
	path := s.find()
	if path == "" {
		return nil, fmt.Errorf("no su binary found")
	}
	shell := s.shell
	if shell == "" {
		shell = "/system/bin/sh"
	}
	user := "root"
	if shellUser {
		user = "shell"
	}
	argv := []string{"-c", command}
	if shellUser {
		argv = []string{user, "-c", command}
	}
	if form == suFormUserSh {
		argv = []string{user, shell, "-c", command}
	}
	cmd := exec.CommandContext(ctx, path, argv...)
	cmd.Dir = cwd
	return startProcessStream(ctx, cmd)
}

type processStream struct {
	cmd         *exec.Cmd
	in          io.WriteCloser
	out, errOut io.ReadCloser
	done        chan struct{}
	code        int
	err         error
	once        sync.Once
}

func startProcessStream(ctx context.Context, cmd *exec.Cmd) (Stream, error) {
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, ow, err := os.Pipe()
	if err != nil {
		in.Close()
		return nil, err
	}
	stderr, ew, err := os.Pipe()
	if err != nil {
		in.Close()
		out.Close()
		ow.Close()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = ow, ew
	err = cmd.Start()
	ow.Close()
	ew.Close()
	if err != nil {
		in.Close()
		out.Close()
		stderr.Close()
		return nil, err
	}
	s := &processStream{cmd: cmd, in: in, out: out, errOut: stderr, done: make(chan struct{})}
	go func() {
		s.err = cmd.Wait()
		if ctx.Err() != nil {
			s.err, s.code = ctx.Err(), -1
		} else if ee, ok := s.err.(*exec.ExitError); ok {
			s.code = ee.ExitCode()
			s.err = nil
		} else if s.err != nil {
			s.code = -1
		}
		close(s.done)
	}()
	return s, nil
}
func (s *processStream) Stdin() io.WriteCloser { return s.in }
func (s *processStream) Stdout() io.ReadCloser { return s.out }
func (s *processStream) Stderr() io.ReadCloser { return s.errOut }
func (s *processStream) Wait() (int, error)    { <-s.done; return s.code, s.err }
func (s *processStream) Close() error {
	s.once.Do(func() { s.in.Close(); s.out.Close(); s.errOut.Close(); _ = s.cmd.Process.Kill(); <-s.done })
	return nil
}

// QuotePOSIX is the channel's shell-word quoting, also used for engine paths.
func QuotePOSIX(s string) string { return quotePOSIX(s) }
