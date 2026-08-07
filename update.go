package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"wanctl/internal/config"
	wanrelease "wanctl/internal/release"
)

// buildVersion is set to an immutable vMAJOR.MINOR.PATCH by the release job.
// Development builds may update to a signed release but cannot claim a version.
var buildVersion = "dev"

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
	fetchAPK := fs.String("fetch-apk", "", "download and verify the Android APK into this directory, print its path, and exit;\n"+
		"\tused by the Android app, which installs it through the system package installer")
	fs.Parse(args)

	if *fetchAPK != "" {
		return fetchAndroidAPK(ctx, *fetchAPK)
	}

	self, err := selfPath()
	if err != nil {
		return fmt.Errorf("locate self: %w", err)
	}
	if real, err := filepath.EvalSymlinks(self); err == nil {
		self = real
	}
	dir := filepath.Dir(self)

	// Checked before the writability probe, which would otherwise send this
	// down splitUpdateViaSudo and report a missing `sudo` — an answer to a
	// question nobody asked.
	if runningFromAPK(self) {
		return errAPKSelfUpdate
	}

	if !canWriteDir(dir) {
		return splitUpdateViaSudo(ctx, self)
	}

	relay := strings.TrimRight(config.EnvOr("WANCTL_RELAY", config.DefaultRelay), "/")
	fmt.Printf("正在验证 %s 的签名发布清单 …\n", relay)
	tmp, version, err := downloadSignedUpdate(ctx, relay, dir, runtime.GOOS, runtime.GOARCH, buildVersion)
	if err != nil {
		return err
	}
	defer os.Remove(tmp) // safe no-op once Rename consumes it

	if err := os.Chmod(tmp, 0o755); err != nil {
		return fmt.Errorf("chmod new binary: %w", err)
	}

	plan := planUpdateRestart(*noRestart, config.ReadPID())
	if plan.stopDetached {
		fmt.Println("正在停止后台 agent …")
		if err := cmdStop(); err != nil {
			return fmt.Errorf("stop daemon: %w", err)
		}
	}

	if err := replaceBinary(tmp, self); err != nil {
		return fmt.Errorf("replace binary at %s: %w", self, err)
	}
	tmp = "" // consumed by Rename

	fmt.Printf("✓ 已安装 wanctl %s: %s\n", version, self)
	reportPATHShadow(self)
	if plan.restartDetached {
		fmt.Println("正在重启后台 agent …")
		if err := cmdStart(); err != nil {
			return fmt.Errorf("restart daemon: %w", err)
		}
	} else if plan.restartManagedPID > 0 {
		fmt.Println("正在通过原 supervisor 重启 agent …")
		if err := restartManagedAgent(self, plan.restartManagedPID); err != nil {
			return err
		}
	}
	return nil
}

var errAPKSelfUpdate = fmt.Errorf(
	"这个 wanctl 由安卓 APK 分发，无法自我升级：APK 里的 lib 目录是系统只读的，" +
		"而 app 能写的目录 Android 一律禁止 exec。请在 wanctl 应用里点「检查更新」，" +
		"或从 relay 的 /dl 下载新 APK 安装。")

// runningFromAPK reports whether this binary is the copy an installed Android
// app carries, as opposed to one someone pushed to /data/local/tmp or installed
// under Termux.
//
// The marker is the layout the package manager creates and only it creates:
// <somewhere under /data/app>/<package>-<suffix>/lib/<abi>/lib*.so. That
// directory is labelled apk_data_file — which is exactly why the binary can run
// from there at all — and it is owned by system:system with no write access for
// the app, so an update that tries to swap the file in place cannot succeed and
// should not be attempted.
func runningFromAPK(self string) bool {
	return runtime.GOOS == "android" && isAPKPath(self)
}

// isAPKPath is the path shape alone, split from the GOOS check so it can be
// tested on the machine this is developed on rather than only on a phone.
func isAPKPath(self string) bool {
	if !strings.HasPrefix(self, "/data/app/") {
		return false
	}
	// .../lib/<abi>/libwanctl.so
	abiDir := filepath.Dir(self)
	return filepath.Base(filepath.Dir(abiDir)) == "lib"
}

// fetchAndroidAPK downloads the APK named by the signed release manifest,
// verifies it, and prints its path — nothing else on stdout, so the caller can
// use the output directly. Being already current is success with no path, not
// an error: the Android app shows "already up to date" for it, and an exit code
// would make that indistinguishable from a network failure.
func fetchAndroidAPK(ctx context.Context, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("prepare %s: %w", dir, err)
	}
	relay := strings.TrimRight(config.EnvOr("WANCTL_RELAY", config.DefaultRelay), "/")
	fmt.Fprintf(os.Stderr, "正在验证 %s 的签名发布清单 …\n", relay)
	tmp, version, err := downloadSignedUpdate(ctx, relay, dir, "android", wanrelease.AndroidAPKArch, buildVersion)
	if err != nil {
		if errors.Is(err, wanrelease.ErrUpToDate) {
			fmt.Fprintf(os.Stderr, "已是最新版本 (%s)\n", buildVersion)
			return nil
		}
		return err
	}
	// The package installer reads the file by path and reports the name it
	// finds, so give it one that says which version the user is approving.
	final := filepath.Join(dir, "wanctl-"+version+".apk")
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("place APK: %w", err)
	}
	if err := os.Chmod(final, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "✓ 已下载并验签 wanctl %s\n", version)
	fmt.Println(final)
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

	plan := planUpdateRestart(false, config.ReadPID())
	if plan.stopDetached {
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

	if plan.restartDetached {
		fmt.Println("正在重启后台 agent …")
		if err := cmdStart(); err != nil {
			return fmt.Errorf("restart daemon: %w", err)
		}
	} else if plan.restartManagedPID > 0 {
		fmt.Println("正在通过原 supervisor 重启 agent …")
		if err := restartManagedAgent(self, plan.restartManagedPID); err != nil {
			return err
		}
	}
	return nil
}

type updateRestartPlan struct {
	stopDetached      bool
	restartDetached   bool
	restartManagedPID int
}

func planUpdateRestart(noRestart bool, pid int) updateRestartPlan {
	return planUpdateRestartWithLiveness(noRestart, pid, processAlive(pid))
}

func planUpdateRestartWithLiveness(noRestart bool, pid int, alive bool) updateRestartPlan {
	if noRestart || !alive {
		return updateRestartPlan{}
	}
	if config.ManagedPID() == pid {
		return updateRestartPlan{restartManagedPID: pid}
	}
	return updateRestartPlan{stopDetached: true, restartDetached: true}
}

// How long to wait for the supervisor to put a new agent in place of the one we
// asked to exit. zyl's Scheduled Task wrapper sleeps 3s between runs; systemd
// units are usually faster.
const (
	managedRestartAttempts = 20
	managedRestartPoll     = time.Second
)

type managedRestartResult int

const (
	managedRestartReplaced managedRestartResult = iota // a different, live agent is registered
	managedRestartStopped                              // the old agent is gone, nothing took over
	managedRestartStuck                                // the old agent is still running
)

// restartManagedAgent asks the supervisor-owned agent to exit and confirms that
// something newer took its place.
//
// Reporting success without that confirmation hid a real upgrade failure: when
// the supervisor runs the agent under another account (a Scheduled Task as
// SYSTEM, a systemd unit as root), a user-owned `wanctl update` cannot terminate
// it. The kill happens in a detached helper whose error goes nowhere, so the
// update printed "正在通过原 supervisor 重启 agent …" and exited 0 while the old
// build kept serving — the new binary on disk made it look done.
func restartManagedAgent(self string, pid int) error {
	if !canTerminatePID(pid) {
		return fmt.Errorf(`新二进制已装好,但运行中的 agent (pid %d) 属于另一个账户(由 supervisor 托管),当前用户无权终止它 —— 旧版本仍在服务。
请以管理员/root 重启那个服务,例如:
  Windows 计划任务  Stop-ScheduledTask -TaskName WanctlAgent; Start-ScheduledTask -TaskName WanctlAgent
  systemd           sudo systemctl restart <unit>
  launchd           sudo launchctl kickstart -k system/<label>`, pid)
	}
	if err := scheduleManagedRestart(self, pid); err != nil {
		return fmt.Errorf("restart supervised agent: %w", err)
	}
	switch awaitManagedRestart(pid, processAlive, config.ReadPID, managedRestartAttempts, managedRestartPoll, time.Sleep) {
	case managedRestartReplaced:
		fmt.Println("✓ agent 已重启,新版本生效")
		return nil
	case managedRestartStopped:
		return fmt.Errorf(`旧 agent (pid %d) 已停止,但 %s 内没有新 agent 接上 —— 这台机器现在没有 agent 在跑。
如果它由「登录时触发」的任务托管,重新登录或手动启动该服务;或者跑 wanctl start 先把它拉起来`,
			pid, time.Duration(managedRestartAttempts)*managedRestartPoll)
	default:
		return fmt.Errorf(`旧 agent (pid %d) 在 %s 后仍在运行,新二进制没有生效。
手动重启它托管的服务,然后用 wanctl status 确认 pid 变了`,
			pid, time.Duration(managedRestartAttempts)*managedRestartPoll)
	}
}

// awaitManagedRestart polls until a live agent other than oldPID is registered,
// then reports what it saw. Clock and probes are injected so the decision table
// is testable without real processes.
func awaitManagedRestart(oldPID int, alive func(int) bool, currentPID func() int, attempts int, poll time.Duration, sleep func(time.Duration)) managedRestartResult {
	for attempt := 0; ; attempt++ {
		if pid := currentPID(); pid > 0 && pid != oldPID && alive(pid) {
			return managedRestartReplaced
		}
		if attempt >= attempts {
			break
		}
		sleep(poll)
	}
	if alive(oldPID) {
		return managedRestartStuck
	}
	return managedRestartStopped
}

// scheduleManagedRestart starts the freshly installed binary outside the
// agent's process tree. The helper waits for the update command's output to be
// relayed, terminates the old agent, and lets its existing supervisor restart
// it with the original flags and identity.
func scheduleManagedRestart(self string, pid int) error {
	cmd := exec.Command(self, "__restart-managed", strconv.Itoa(pid))
	cmd.SysProcAttr = detachSysProcAttr()
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func cmdRestartManaged(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("internal managed restart expects one pid")
	}
	pid, err := strconv.Atoi(args[0])
	if err != nil || pid <= 0 {
		return fmt.Errorf("invalid managed agent pid %q", args[0])
	}
	time.Sleep(time.Second)
	if config.ReadPID() != pid || config.ManagedPID() != pid || !processAlive(pid) {
		return nil
	}
	return terminatePID(pid)
}

// pathShadow reports the wanctl that a bare `wanctl` resolves to, when that is
// not the copy this update just replaced. An empty result means there is
// nothing to say: either the updated copy is the one that wins, or no bare
// `wanctl` resolves at all because the user invokes it by path.
//
// This replaced a routine that walked $PATH and deleted every *other* file
// named `wanctl`. Deleting was wrong twice over. A basename match is not an
// identification — a wrapper script that sets WANCTL_RELAY, a developer's own
// build, or a distro-packaged file called `wanctl` was removed without ever
// being read. And the root-owned-directory case re-execs this command under
// sudo (see splitUpdateViaSudo), so the deletion ran as root, where the "it
// fails with permission denied and we merely print a hint" mitigation the old
// code leaned on can never fire: `sudo wanctl update` removed /usr/bin/wanctl
// outright and whatever put it there found out later.
//
// Shadowing is the only thing worth reporting, because it is the only thing
// that changes what the user gets. A copy that loses the PATH race is inert,
// and hunting down inert files is not this command's business.
func pathShadow(self string) string {
	// LookPath is the resolution itself rather than a reimplementation of it:
	// it applies the executable bit on Unix and PATHEXT on Windows, so its
	// answer is the one the user's shell would give.
	found, lookErr := exec.LookPath("wanctl")
	// ErrDot still yields a usable path — PATH contains "." and a bare command
	// name really would run that file.
	if lookErr != nil && !errors.Is(lookErr, exec.ErrDot) {
		return ""
	}
	// SameFile rather than string comparison, so a symlink, a hard link or a
	// bind mount onto the updated binary is correctly read as "no shadow".
	selfInfo, err := os.Stat(self)
	if err != nil {
		return ""
	}
	foundInfo, err := os.Stat(found)
	if err != nil {
		return ""
	}
	if os.SameFile(selfInfo, foundInfo) {
		return ""
	}
	return found
}

// reportPATHShadow tells the user when the binary they just upgraded is not the
// one a bare `wanctl` will run, and stops there. It names the command that
// answers what the other copy is, because this cannot tell them itself.
func reportPATHShadow(self string) {
	other := pathShadow(self)
	if other == "" {
		return
	}
	fmt.Fprintf(os.Stderr,
		"提示: 升级的是 %s,但 PATH 上先命中的是 %s —— 直接敲 `wanctl` 仍会跑到那一个。\n"+
			"  先看看它是什么:  %s --version\n"+
			"  确认是旧版后自行删除,或把 %s 排到 PATH 前面。\n",
		self, other, other, filepath.Dir(self))
}

func downloadSignedUpdate(ctx context.Context, relay, dir, goos, goarch, currentVersion string) (string, string, error) {
	manifestRaw, err := fetchLimited(ctx, relay+"/dl/"+wanrelease.ManifestName, wanrelease.MaxManifestSize)
	if err != nil {
		return "", "", err
	}
	signatureRaw, err := fetchLimited(ctx, relay+"/dl/"+wanrelease.SignatureName, 4096)
	if err != nil {
		return "", "", err
	}
	manifest, err := wanrelease.VerifyManifest(manifestRaw, signatureRaw, wanrelease.TrustedPublicKeys)
	if err != nil {
		return "", "", fmt.Errorf("verify release manifest: %w", err)
	}
	artifact, err := wanrelease.Select(manifest, goos, goarch, currentVersion)
	if err != nil {
		return "", "", err
	}
	url := relay + "/dl/" + artifact.Name
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", "", err
	}
	cl := &http.Client{Timeout: 5 * time.Minute}
	resp, err := cl.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("fetch %s: %s", url, resp.Status)
	}
	if resp.ContentLength > artifact.Size {
		return "", "", fmt.Errorf("artifact content length %d exceeds signed size %d", resp.ContentLength, artifact.Size)
	}

	f, err := os.CreateTemp(dir, "wanctl-update-*.tmp")
	if err != nil {
		return "", "", fmt.Errorf("create tempfile in %s: %w", dir, err)
	}
	if err := wanrelease.VerifyArtifact(resp.Body, f, artifact); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", "", fmt.Errorf("verify downloaded %s: %w", artifact.Name, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", "", fmt.Errorf("sync downloaded artifact: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", "", err
	}
	return f.Name(), manifest.Version, nil
}

func fetchLimited(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: time.Minute}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: %s", url, resp.Status)
	}
	if resp.ContentLength > limit {
		return nil, fmt.Errorf("fetch %s: response too large", url)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("fetch %s: response too large", url)
	}
	return raw, nil
}
