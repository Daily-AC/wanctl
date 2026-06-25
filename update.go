package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
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
func cmdUpdate(ctx context.Context) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate self: %w", err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return fmt.Errorf("resolve symlinks: %w", err)
	}

	binName := fmt.Sprintf("wanctl-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	relay := strings.TrimRight(config.EnvOr("WANCTL_RELAY", config.DefaultRelay), "/")
	url := relay + "/dl/" + binName

	fmt.Printf("下载 %s …\n", url)
	tmp, err := downloadToTemp(ctx, url, filepath.Dir(self))
	if err != nil {
		return err
	}
	defer os.Remove(tmp) // safe no-op once Rename consumes it

	if err := os.Chmod(tmp, 0o755); err != nil {
		return fmt.Errorf("chmod new binary: %w", err)
	}

	wasRunning := processAlive(config.ReadPID())
	if wasRunning {
		fmt.Println("正在停止后台 agent …")
		if err := cmdStop(); err != nil {
			return fmt.Errorf("stop daemon: %w", err)
		}
	}

	if err := replaceBinary(tmp, self); err != nil {
		return fmt.Errorf("replace binary at %s: %w (提示: 用 sudo wanctl update 或把二进制放到自己有写权限的目录)", self, err)
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
