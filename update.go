package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"wanctl/internal/config"
)

// cmdUpdate replaces the running wanctl binary with the latest one served by
// the relay's /dl/<bin> endpoint. If the background daemon is running, it is
// stopped before the swap and restarted after — so users keep their session.
//
// On Unix the swap is a tempfile-in-same-dir + rename, which is atomic and safe
// even though the binary is currently executing (the kernel keeps the old inode
// alive for the running process). On Windows the running .exe can't be renamed,
// so we rename it to <name>.old first, then write the new file in place.
//
// When the binary lives in a root-owned dir (e.g. /usr/local/bin), we split the
// work in two: the user process stops the daemon, re-execs `sudo wanctl update
// --no-restart` (which only does download + swap as root), then the user
// process starts the daemon again — so the daemon process keeps running as the
// original user, not as root.
func cmdUpdate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	noRestart := fs.Bool("no-restart", false, "internal: skip daemon stop/start (used by the sudo-elevated phase)")
	fs.Parse(args)

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate self: %w", err)
	}
	if real, err := filepath.EvalSymlinks(self); err == nil {
		self = real
	}
	dir := filepath.Dir(self)

	if !canWriteDir(dir) {
		return splitUpdateViaSudo(ctx, self)
	}

	binName := fmt.Sprintf("wanctl-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	relay := strings.TrimRight(config.EnvOr("WANCTL_RELAY", config.DefaultRelay), "/")
	url := relay + "/dl/" + binName

	fmt.Printf("下载 %s …\n", url)
	tmp, err := downloadToTemp(ctx, url, dir)
	if err != nil {
		return err
	}
	defer os.Remove(tmp) // safe no-op once Rename consumes it

	if err := os.Chmod(tmp, 0o755); err != nil {
		return fmt.Errorf("chmod new binary: %w", err)
	}

	wasRunning := !*noRestart && processAlive(config.ReadPID())
	if wasRunning {
		fmt.Println("正在停止后台 agent …")
		if err := cmdStop(); err != nil {
			return fmt.Errorf("stop daemon: %w", err)
		}
	}

	if err := replaceBinary(tmp, self); err != nil {
		return fmt.Errorf("replace binary at %s: %w", self, err)
	}
	tmp = "" // consumed by Rename

	fmt.Printf("✓ 已替换 %s\n", self)
	pruneStaleCopies(self)
	if wasRunning {
		fmt.Println("正在重启后台 agent …")
		if err := cmdStart(); err != nil {
			return fmt.Errorf("restart daemon: %w", err)
		}
	}
	return nil
}

// canWriteDir reports whether the current process can create files in dir
// (the most reliable permission check on POSIX: try it).
func canWriteDir(dir string) bool {
	f, err := os.CreateTemp(dir, ".wanctl-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

// splitUpdateViaSudo handles the root-owned-dir case: stop daemon as user,
// re-exec `sudo wanctl update --no-restart` (root just swaps the binary),
// then restart the daemon as the original user. This keeps the long-running
// daemon owned by the user — running it as root would change file ownership of
// the config dir / pid file / logs.
func splitUpdateViaSudo(ctx context.Context, self string) error {
	if runtime.GOOS == "windows" {
		return fmt.Errorf("升级 %s 需要管理员权限。请用「以管理员身份运行」打开终端再跑 wanctl update", self)
	}
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return fmt.Errorf("升级 %s 需要 root 权限,但本机找不到 sudo。请用 root 身份直接跑: wanctl update", self)
	}

	wasRunning := processAlive(config.ReadPID())
	if wasRunning {
		fmt.Println("正在停止后台 agent …")
		if err := cmdStop(); err != nil {
			return fmt.Errorf("stop daemon: %w", err)
		}
	}

	fmt.Printf("wanctl: %s 需要 sudo 才能替换,请在下方提示输入密码 …\n", filepath.Dir(self))
	cmd := exec.CommandContext(ctx, sudo, self, "update", "--no-restart")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sudo wanctl update: %w", err)
	}

	if wasRunning {
		fmt.Println("正在重启后台 agent …")
		if err := cmdStart(); err != nil {
			return fmt.Errorf("restart daemon: %w", err)
		}
	}
	return nil
}

// pruneStaleCopies walks $PATH and deletes any *other* wanctl binary it finds,
// so a bare `wanctl` in the user's shell always resolves to the freshly-updated
// copy. Failures are reported as hints (most commonly permission denied — the
// other copy is in /usr/local/bin and we're running from ~/.local/bin or vice
// versa). We never `sudo` automatically; we just tell the user.
func pruneStaleCopies(self string) {
	binName := "wanctl"
	if runtime.GOOS == "windows" {
		binName = "wanctl.exe"
	}
	selfReal, _ := filepath.EvalSymlinks(self)
	if selfReal == "" {
		selfReal = self
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, binName)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		candReal, _ := filepath.EvalSymlinks(candidate)
		if candReal == "" {
			candReal = candidate
		}
		if candReal == selfReal {
			continue
		}
		if err := os.Remove(candidate); err == nil {
			fmt.Printf("清理旧版: %s\n", candidate)
		} else {
			fmt.Fprintf(os.Stderr, "提示: 还有一个 wanctl 在 %s 删不掉 (%v)。\n  手动跑: sudo rm %s\n  否则 bare `wanctl` 可能跑到旧版。\n", candidate, err, candidate)
		}
	}
}

// downloadToTemp streams url to a *.tmp file in dir (so the eventual Rename to
// the final path stays atomic — same filesystem) and returns the temp path.
func downloadToTemp(ctx context.Context, url, dir string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	cl := &http.Client{Timeout: 5 * time.Minute}
	resp, err := cl.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %s: %s", url, resp.Status)
	}

	f, err := os.CreateTemp(dir, "wanctl-update-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create tempfile in %s: %w", dir, err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("write tempfile: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
