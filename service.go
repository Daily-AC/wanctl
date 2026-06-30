package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"wanctl/internal/config"
)

// cmdService installs, removes, or reports an OS-native autostart unit for the
// agent so it survives the terminal closing AND a reboot — a real service
// instead of the bare `wanctl start` detach (which on Windows dies with its
// console, and on macOS/Linux dies with the login session). The unit just runs
// `<wanctl> agent`, which reads its token from the config dir, so no secrets are
// baked into it.
func cmdService(ctx context.Context, args []string) error {
	action := "status"
	if len(args) > 0 {
		action = args[0]
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate wanctl binary: %w", err)
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		self, _ = os.Executable()
	}

	switch action {
	case "install":
		if config.EnvOr("WANCTL_TOKEN", config.StoredToken()) == "" {
			return fmt.Errorf("not logged in yet — run `wanctl` (device) or `wanctl login` first so the service has a token")
		}
		return serviceInstall(self)
	case "uninstall", "remove":
		return serviceUninstall()
	case "status":
		return serviceStatus()
	default:
		return fmt.Errorf("usage: wanctl service install|uninstall|status")
	}
}

// run executes a command, returning combined output for diagnostics.
func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// --- Linux: systemd user unit ---

func linuxUnitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", "wanctl.service"), nil
}

func linuxInstall(self string) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("systemctl not found; this auto-installer needs systemd. Run `%s agent` from your own init/supervisor instead", self)
	}
	unit, err := linuxUnitPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		return err
	}
	body := fmt.Sprintf(`[Unit]
Description=wanctl agent (remote device control)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s agent
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
`, self)
	if err := os.WriteFile(unit, []byte(body), 0o644); err != nil {
		return err
	}
	if _, err := run("systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("daemon-reload: %w", err)
	}
	if out, err := run("systemctl", "--user", "enable", "--now", "wanctl.service"); err != nil {
		return fmt.Errorf("enable wanctl.service: %v\n%s", err, out)
	}
	// Best-effort: keep the service up after logout / across reboot without an
	// interactive login. Needs polkit/root, so don't fail the install if denied.
	if u, e := user.Current(); e == nil {
		if out, err := run("loginctl", "enable-linger", u.Username); err != nil {
			fmt.Fprintf(os.Stderr, "note: `loginctl enable-linger %s` failed (%v); the service still runs while you're logged in. Run it as root for boot-without-login: sudo loginctl enable-linger %s\n", u.Username, err, u.Username)
			_ = out
		}
	}
	fmt.Printf("✓ installed systemd user service → %s\n  status: systemctl --user status wanctl\n  logs:   journalctl --user -u wanctl -f\n", unit)
	return nil
}

func linuxUninstall() error {
	run("systemctl", "--user", "disable", "--now", "wanctl.service")
	unit, err := linuxUnitPath()
	if err == nil {
		os.Remove(unit)
		run("systemctl", "--user", "daemon-reload")
	}
	fmt.Println("✓ removed systemd user service")
	return nil
}

func linuxStatus() error {
	out, _ := run("systemctl", "--user", "is-active", "wanctl.service")
	unit, _ := linuxUnitPath()
	installed := "no"
	if _, err := os.Stat(unit); err == nil {
		installed = unit
	}
	fmt.Printf("service (systemd --user): installed=%s active=%s\n", installed, out)
	return nil
}

// --- macOS: launchd LaunchAgent ---

const macLabel = "com.wanctl.agent"

func macPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", macLabel+".plist"), nil
}

func macInstall(self string) error {
	plist, err := macPlistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return err
	}
	logPath, _ := config.LogPath()
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>agent</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, macLabel, self, logPath, logPath)
	if err := os.WriteFile(plist, []byte(body), 0o644); err != nil {
		return err
	}
	// Reload: unload an old copy first (ignore error), then load enabled.
	run("launchctl", "unload", plist)
	if out, err := run("launchctl", "load", "-w", plist); err != nil {
		return fmt.Errorf("launchctl load: %v\n%s", err, out)
	}
	fmt.Printf("✓ installed launchd agent → %s\n  status: launchctl list | grep %s\n  logs:   tail -f %s\n", plist, macLabel, logPath)
	return nil
}

func macUninstall() error {
	plist, err := macPlistPath()
	if err == nil {
		run("launchctl", "unload", "-w", plist)
		os.Remove(plist)
	}
	fmt.Println("✓ removed launchd agent")
	return nil
}

func macStatus() error {
	plist, _ := macPlistPath()
	installed := "no"
	if _, err := os.Stat(plist); err == nil {
		installed = plist
	}
	out, _ := run("launchctl", "list")
	active := "no"
	if strings.Contains(out, macLabel) {
		active = "yes"
	}
	fmt.Printf("service (launchd): installed=%s loaded=%s\n", installed, active)
	return nil
}

// --- Windows: scheduled task ---

const winTaskName = "WanctlAgent"

func winInstall(self string) error {
	// ONLOGON task running the agent, recreated (/f) if it exists. Survives the
	// console closing and re-runs at every logon (i.e. across reboots). The agent
	// itself loops/reconnects, so it rarely exits on its own.
	tr := fmt.Sprintf(`"%s" agent`, self)
	if out, err := run("schtasks", "/create", "/tn", winTaskName, "/tr", tr,
		"/sc", "onlogon", "/rl", "limited", "/f"); err != nil {
		return fmt.Errorf("schtasks create: %v\n%s", err, out)
	}
	// Start it now so the user doesn't have to log out/in.
	run("schtasks", "/run", "/tn", winTaskName)
	fmt.Printf("✓ installed scheduled task %q (runs at logon + started now)\n  status: schtasks /query /tn %s\n", winTaskName, winTaskName)
	return nil
}

func winUninstall() error {
	run("schtasks", "/end", "/tn", winTaskName)
	if out, err := run("schtasks", "/delete", "/tn", winTaskName, "/f"); err != nil {
		return fmt.Errorf("schtasks delete: %v\n%s", err, out)
	}
	fmt.Println("✓ removed scheduled task")
	return nil
}

func winStatus() error {
	out, err := run("schtasks", "/query", "/tn", winTaskName)
	if err != nil {
		fmt.Println("service (scheduled task): installed=no")
		return nil
	}
	fmt.Printf("service (scheduled task %q):\n%s\n", winTaskName, out)
	return nil
}

// --- dispatch by OS ---

func serviceInstall(self string) error {
	switch runtime.GOOS {
	case "linux":
		return linuxInstall(self)
	case "darwin":
		return macInstall(self)
	case "windows":
		return winInstall(self)
	}
	return fmt.Errorf("`wanctl service` is not supported on %s; run `%s agent` from your own supervisor", runtime.GOOS, self)
}

func serviceUninstall() error {
	switch runtime.GOOS {
	case "linux":
		return linuxUninstall()
	case "darwin":
		return macUninstall()
	case "windows":
		return winUninstall()
	}
	return fmt.Errorf("not supported on %s", runtime.GOOS)
}

func serviceStatus() error {
	switch runtime.GOOS {
	case "linux":
		return linuxStatus()
	case "darwin":
		return macStatus()
	case "windows":
		return winStatus()
	}
	return fmt.Errorf("not supported on %s", runtime.GOOS)
}
