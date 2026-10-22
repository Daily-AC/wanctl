package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"wanctl/internal/opencu"
	"wanctl/internal/protocol"
)

const OpenCUInstallerURL = "https://github.com/Daily-AC/opencu/releases/latest/download/install.ps1"
const installTimeout = 5 * time.Minute
const maxInstallerBytes = 1 << 20
const maxInstallerOutput = 16 << 10

// InstallOpenCU runs the fixed installer in the console user's existing helper
// transport. No script, path, command or URL arrives from the portal.
func InstallOpenCU(ctx context.Context, installing func()) (string, error) {
	return installOpenCUWith(ctx, startInstallHelper, installing)
}
func installOpenCUWith(ctx context.Context, start helperStart, installing func()) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, installTimeout+5*time.Second)
	defer cancel()
	child, err := start(ctx)
	if err != nil {
		return "", err
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { child.stdin.Close(); _ = child.kill(); child.stdout.Close(); close(done) })
	defer func() {
		if !stop() {
			<-done
		}
		child.stdin.Close()
		child.stdout.Close()
		_ = child.kill()
		_ = child.wait()
	}()
	raw, _ := json.Marshal(Job{Action: "install_opencu"})
	if err := protocol.WriteFrame(child.stdin, protocol.FrameJSON, raw); err != nil {
		return "", err
	}
	for {
		m, err := protocol.ReadMessage(child.stdout)
		if ctx.Err() != nil {
			return "", errors.New("OpenCU installation timed out; check the device logs")
		}
		if err != nil {
			return "", fmt.Errorf("OpenCU installer helper: %w", err)
		}
		var p protocol.InstallProgress
		if m.Kind != protocol.KindInstallProgress || json.Unmarshal(m.Data, &p) != nil {
			return "", errors.New("invalid OpenCU installer reply")
		}
		switch p.Stage {
		case "installing":
			installing()
		case "done":
			return p.Version, nil
		case "failed":
			return "", errors.New(cleanInstallerError(p.Reason))
		default:
			return "", errors.New("invalid OpenCU installer stage")
		}
	}
}

func installHelperMain(in io.Reader, out io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), installTimeout)
	defer cancel()
	go func() { var b [1]byte; _, _ = in.Read(b[:]); cancel() }()
	send := func(stage, version, reason string) {
		raw, _ := json.Marshal(protocol.InstallProgress{Package: "opencu", Version: version, Stage: stage, Reason: reason})
		_ = protocol.WriteMessage(out, protocol.Message{Kind: protocol.KindInstallProgress, Data: raw})
	}
	err := checkInstallSession()
	var version string
	if err == nil {
		version, err = runOpenCUInstaller(ctx, downloadOpenCUInstaller, executeOpenCUInstaller, opencu.InstalledVersion, func() { send("installing", "", "") })
	}
	if err != nil {
		send("failed", "", cleanInstallerError(err.Error()))
	} else {
		send("done", version, "")
	}
	return 0
}

func downloadOpenCUInstaller(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, OpenCUInstallerURL, nil)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Timeout: time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 || req.URL.Scheme != "https" {
			return errors.New("installer redirect must use HTTPS")
		}
		return nil
	}}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("official OpenCU installer download: HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxInstallerBytes+1))
	if err == nil && (len(raw) == 0 || len(raw) > maxInstallerBytes) {
		err = errors.New("official OpenCU installer is empty or too large")
	}
	return raw, err
}

func runOpenCUInstaller(ctx context.Context, download func(context.Context) ([]byte, error), run func(context.Context, []byte) (string, error), version func() string, installing func()) (string, error) {
	script, err := download(ctx)
	if err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	installing()
	output, err := run(ctx, script)
	if ctx.Err() != nil {
		return "", errors.New("OpenCU installation timed out; check the device logs")
	}
	if err != nil {
		reason := cleanInstallerError(output)
		if reason == "" {
			reason = "OpenCU installer exited unsuccessfully"
		}
		return "", errors.New(reason)
	}
	v := version()
	if v == "" {
		return "", errors.New("OpenCU installer finished but no installed version could be read")
	}
	return v, nil
}

// Keep the tail, so the installer's own final error survives verbose output.
type installerOutput struct {
	mu   sync.Mutex
	data []byte
}

func (b *installerOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if len(p) > maxInstallerOutput {
		p = p[len(p)-maxInstallerOutput:]
	}
	b.data = append(b.data, p...)
	if len(b.data) > maxInstallerOutput {
		b.data = b.data[len(b.data)-maxInstallerOutput:]
	}
	return n, nil
}

var installerANSI = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

func cleanInstallerError(s string) string {
	s = installerANSI.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	s = ""
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			s = lines[i]
			break
		}
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	r := []rune(strings.TrimSpace(s))
	if len(r) > 400 {
		r = r[:400]
	}
	return string(r)
}
func executeOpenCUInstaller(ctx context.Context, script []byte) (string, error) {
	dir, err := os.MkdirTemp("", "wanctl-opencu-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "install.ps1")
	if err = os.WriteFile(path, script, 0600); err != nil {
		return "", err
	}
	// Catch the terminating error ourselves; PowerShell's default formatted
	// stderr ends with stack/category noise instead of the useful error message.
	command := "$ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue'; [Console]::OutputEncoding=[Text.UTF8Encoding]::new(); try { & '" + strings.ReplaceAll(path, "'", "''") + "' } catch { [Console]::Error.WriteLine($_.Exception.Message); exit 1 }"
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", command)
	helperProcessAttrs(cmd)
	var output installerOutput
	cmd.Stdout = &output
	cmd.Stderr = &output
	cmd.WaitDelay = 2 * time.Second
	err = cmd.Run()
	return string(output.data), err
}
