package desktop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"wanctl/internal/elevate"
	"wanctl/internal/opencu"
	"wanctl/internal/protocol"
)

// AndroidOpenCU shares the cache across all controller sessions on one APK.
// The native launcher is injected through the existing elevation manager.
type AndroidOpenCU struct {
	Cache     *opencu.AndroidCache
	Elevator  *elevate.Manager
	OnVersion func(string)
}

type openCUStartError struct{ code, message string }

func (e *openCUStartError) Error() string            { return e.message }
func androidStartError(code string, err error) error { return &openCUStartError{code, err.Error()} }

func (a *AndroidOpenCU) StartOpenCU(ctx context.Context) (OpenCUConnection, error) {
	fail := func(err error) (OpenCUConnection, error) { return nil, androidStartError("OPENCU_START_FAILED", err) }
	if a.Elevator == nil {
		return fail(errors.New("turn on 提权通道 in the wanctl app"))
	}
	channel, _, err := a.Elevator.Select(ctx, "")
	if err != nil {
		return fail(err)
	}
	via := channel.Kind() // Placement and start cannot silently switch channels.
	engine, release, err := a.Cache.Acquire(ctx)
	if err != nil {
		return nil, androidStartError("OPENCU_NOT_INSTALLED", err)
	}
	keep := false
	defer func() {
		if !keep {
			release()
		}
	}()
	if a.OnVersion != nil {
		a.OnVersion(engine.Manifest.Version)
	}
	path, err := a.place(ctx, via, engine)
	if err != nil {
		return nil, androidStartError("OPENCU_NOT_INSTALLED", fmt.Errorf("OPENCU_NOT_INSTALLED: Android engine placement/verification: %w", err))
	}
	initCtx, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	// The request context must not kill a successfully initialized persistent
	// session when DoOpenCU returns. Its own Close owns the stream from then on.
	lifetime, cancel := context.WithCancel(context.Background())
	stopCancel := context.AfterFunc(initCtx, cancel)
	defer stopCancel()
	stream, err := a.Elevator.ShellStream(lifetime, via, "CLASSPATH="+elevate.QuotePOSIX(path)+" exec app_process / dev.opencu.android.Main mcp", "")
	if err != nil {
		cancel()
		return fail(err)
	}
	diagnostic := &firstDiagnostic{}
	drained := make(chan struct{})
	go func() { _, _ = io.Copy(diagnostic, stream.Stderr()); close(drained) }()
	done := make(chan struct{})
	go func() { _, _ = stream.Wait(); close(done) }()
	peer, err := opencu.StartPipes(initCtx, stream.Stdin(), stream.Stdout(), done, func() { cancel(); _ = stream.Close() })
	if err != nil {
		cancel()
		stream.Close()
		<-drained
		detail := diagnostic.String()
		if detail != "" {
			err = fmt.Errorf("%w: engine stderr: %s", err, detail)
		}
		return fail(err)
	}
	if !stopCancel() && initCtx.Err() != nil {
		peer.Close()
		cancel()
		stream.Close()
		return fail(initCtx.Err())
	}
	c := &androidOpenCUConnection{peer: peer, stream: stream, cancel: cancel, release: release}
	keep = true
	// Only remove files matching our exact hash-name shape, after successful
	// initialization. Cleanup is best effort and never changes an MCP outcome.
	cleanupCtx, cleanupCancel := context.WithTimeout(ctx, 5*time.Second)
	a.removeOld(cleanupCtx, via, path)
	cleanupCancel()
	return c, nil
}

type androidOpenCUConnection struct {
	peer    *opencu.Client
	stream  elevate.Stream
	cancel  context.CancelFunc
	release func()
	once    sync.Once
}

func (c *androidOpenCUConnection) Close() {
	c.once.Do(func() { c.peer.Close(); c.cancel(); c.stream.Close(); c.release() })
}
func (c *androidOpenCUConnection) Do(ctx context.Context, req protocol.DesktopRequest) (protocol.OpenCUResult, error) {
	var r protocol.OpenCUResult
	var err error
	if req.Discover {
		r.Discovery, err = c.peer.Discover(ctx)
	} else {
		r.Result, err = c.peer.Call(ctx, req.Tool, req.Arguments)
	}
	var rpc *opencu.RPCError
	if errors.As(err, &rpc) {
		r.RPCError = rpc.Raw
		r.Message = rpc.Error()
		err = nil
	}
	return r, err
}

// The diagnostic is drained in full, retaining at most 512 bytes of its first
// line. Quoting escapes control/format characters and invalid UTF-8 bytes.
// It is never included in audit/notification summaries.
type firstDiagnostic struct {
	mu    sync.Mutex
	line  []byte
	ended bool
}

func (d *firstDiagnostic) Write(b []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := len(b)
	if !d.ended {
		if i := strings.IndexByte(string(b), '\n'); i >= 0 {
			b = b[:i]
			d.ended = true
		}
		left := 512 - len(d.line)
		d.line = append(d.line, b[:min(len(b), left)]...)
		if len(d.line) == 512 {
			d.ended = true
		}
	}
	return n, nil
}
func (d *firstDiagnostic) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.line) == 0 {
		return ""
	}
	return strconv.QuoteToASCII(string(d.line))
}

// shellCommand streams bytes, never a base64 command or an app-private path.
// Both outputs are drained, bounded, and wait failure is never retried.
func (a *AndroidOpenCU) shellCommand(ctx context.Context, via elevate.Kind, command string, input io.Reader) (string, int, error) {
	s, err := a.Elevator.ShellStream(ctx, via, command, "")
	if err != nil {
		return "", -1, err
	}
	defer s.Close()
	out, diag := &limitedOutput{}, &firstDiagnostic{}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(out, s.Stdout()) }()
	go func() { defer wg.Done(); _, _ = io.Copy(diag, s.Stderr()) }()
	if input != nil {
		_, err = io.Copy(s.Stdin(), input)
	}
	if err == nil {
		// A short command may already have exited before close-stdin is
		// acknowledged. Its exit packet is authoritative in that race.
		_ = s.Stdin().Close()
	}
	if err != nil {
		s.Close()
	}
	code, waitErr := s.Wait()
	wg.Wait()
	if err == nil {
		err = waitErr
	}
	if err == nil && code != 0 {
		err = fmt.Errorf("Android engine command exited %d: %s", code, diag.String())
	}
	return out.String(), code, err
}

type limitedOutput struct{ b []byte }

func (o *limitedOutput) Write(b []byte) (int, error) {
	n := len(b)
	o.b = append(o.b, b[:min(len(b), 4096-len(o.b))]...)
	return n, nil
}
func (o *limitedOutput) String() string { return string(o.b) }
func (a *AndroidOpenCU) deviceHash(ctx context.Context, via elevate.Kind, path string) (string, error) {
	out, _, err := a.shellCommand(ctx, via, "test ! -L "+elevate.QuotePOSIX(path)+" && test -f "+elevate.QuotePOSIX(path)+" && sha256sum "+elevate.QuotePOSIX(path), nil)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return "", errors.New("invalid device sha256sum output")
	}
	return fields[0], nil
}
func (a *AndroidOpenCU) place(ctx context.Context, via elevate.Kind, e opencu.AndroidEngine) (string, error) {
	// The cache validated the full lowercase digest. Validate again at this
	// shell boundary so even a malformed internal caller cannot inject a path.
	hash := e.Manifest.SHA256
	if len(hash) != 64 || strings.Trim(hash, "0123456789abcdef") != "" {
		return "", errors.New("invalid Android engine SHA-256")
	}
	// The bytes the cache verified, bounded by the manifest size; the cache
	// file is never reopened here.
	jar, err := e.Reader()
	if err != nil {
		return "", err
	}
	path := "/data/local/tmp/opencu-android-" + hash[:16] + ".jar"
	if got, err := a.deviceHash(ctx, via, path); err == nil && got == hash {
		return path, nil
	}
	// mktemp creates exclusively, so a pre-existing symlink can never redirect
	// privileged writes. The source path is not sent to shell (SELinux forbids it).
	q := elevate.QuotePOSIX(path)
	command := `t=$(mktemp /data/local/tmp/.opencu-android-XXXXXXXX) || exit 1; trap 'rm -f "$t"' EXIT; cat > "$t" && chmod 0644 "$t" && test ! -L ` + q + " && { test ! -e " + q + " || test -f " + q + `; } && mv -f "$t" ` + q
	if _, _, err = a.shellCommand(ctx, via, command, jar); err != nil {
		return "", fmt.Errorf("place Android engine: %w", err)
	}
	got, err := a.deviceHash(ctx, via, path)
	if err != nil {
		return "", fmt.Errorf("verify Android engine on device: %w", err)
	}
	if got != hash {
		return "", errors.New("Android engine SHA-256 mismatch on device")
	}
	return path, nil
}
func (a *AndroidOpenCU) removeOld(ctx context.Context, via elevate.Kind, path string) {
	// Exactly sixteen lowercase hex digits; unrelated jars and active peers'
	// current file remain untouched. Old engines already loaded their jar.
	pattern := strings.Repeat("[0-9a-f]", 16)
	command := "for f in /data/local/tmp/opencu-android-" + pattern + ".jar; do [ \"$f\" = " + elevate.QuotePOSIX(path) + " ] || { [ ! -L \"$f\" ] && [ -f \"$f\" ] && rm -f \"$f\"; }; done; :"
	_, _, _ = a.shellCommand(ctx, via, command, nil)
}
