package main

import (
	"context"
	"flag"
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
// agent so it survives the terminal closing and returns with the user's OS
// session (or at boot when the selected OS service manager supports that)
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
		fs := flag.NewFlagSet("service install", flag.ContinueOnError)
		name := fs.String("name", "", "device name to bake into the unit (default: hostname, resolved at every start)")
		portalFPs := fs.String("portal-fps", "", "comma-separated portal admin fingerprints the agent must trust")
		mode := fs.String("mode", "", "policy mode to bake in; omit so the persisted mode (and portal switches) win")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		extra, err := serviceAgentArgs(*name, *portalFPs, *mode)
		if err != nil {
			return err
		}
		if err := serviceInstall(self, extra); err != nil {
			return err
		}
		warnMissingPortalTrust(*portalFPs)
		return nil
	case "uninstall", "remove":
		return serviceUninstall()
	case "status":
		return serviceStatus()
	default:
		return fmt.Errorf("usage: wanctl service install|uninstall|status")
	}
}

// serviceAgentArgs turns the install-time options into the `wanctl agent`
// arguments the unit will carry. They are baked in because a unit is what runs
// after a reboot, when nobody is at a terminal to re-supply them: a name left
// out silently degrades to the hostname, and portal fingerprints left out leave
// the device unable to accept portal-side decisions at all.
//
// --mode is deliberately available but not defaulted: baking a mode in makes the
// unit outrank the persisted mode, so a portal-side switch is undone by the next
// restart.
func serviceAgentArgs(name, portalFPs, mode string) ([]string, error) {
	var extra []string
	if name != "" {
		extra = append(extra, "--name", name)
	}
	if portalFPs != "" {
		if _, err := config.ParsePortalFingerprints(portalFPs); err != nil {
			return nil, fmt.Errorf("--portal-fps: %w", err)
		}
		extra = append(extra, "--portal-fps", portalFPs)
	}
	if mode != "" {
		if mode != "normal" && mode != "bypass" {
			return nil, fmt.Errorf("--mode: want normal or bypass, got %q", mode)
		}
		extra = append(extra, "--mode", mode)
	}
	return extra, nil
}

// warnMissingPortalTrust reports the one misconfiguration whose only symptom is
// a bare 502 in someone else's browser: with no portal admin fingerprint the
// agent refuses the portal's console session, so clicking "trust" (or any
// approval) on the portal fails without ever reaching this device.
func warnMissingPortalTrust(portalFPs string) {
	if portalFPs != "" {
		return
	}
	admins, err := config.OpenPortalAdmins()
	if err != nil || len(admins.List()) > 0 {
		return
	}
	fmt.Fprintln(os.Stderr, "wanctl: warning — no portal admin fingerprint is configured on this device.")
	fmt.Fprintln(os.Stderr, "  The portal cannot open a console session here, so approving this device's")
	fmt.Fprintln(os.Stderr, "  pairings or requests from the web UI will fail with a 502.")
	fmt.Fprintln(os.Stderr, "  Fix: wanctl service install --portal-fps SHA256:...")
	fmt.Fprintln(os.Stderr, "  The fingerprint is the portal's `identity:` line, also present in")
	fmt.Fprintln(os.Stderr, "  portal_admins.json on any already-working device.")
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

// systemdArgs renders arguments for an ExecStart line. systemd splits on
// whitespace unless the argument is quoted, so anything with a space (a device
// name like "lab box") would otherwise become two arguments.
func systemdArgs(extra []string) string {
	var b strings.Builder
	for _, a := range extra {
		b.WriteByte(' ')
		if strings.ContainsAny(a, " \t\"\\") {
			b.WriteString(`"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(a) + `"`)
			continue
		}
		b.WriteString(a)
	}
	return b.String()
}

func linuxInstall(self string, extra []string) error {
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
ExecStart=%s agent --managed%s
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
`, self, systemdArgs(extra))
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

func macInstall(self string, extra []string) error {
	plist, err := macPlistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return err
	}
	logPath, _ := config.LogPath()
	var extraXML strings.Builder
	for _, a := range extra {
		extraXML.WriteString("\n    <string>" + xmlEscape(a) + "</string>")
	}
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>agent</string>
    <string>--managed</string>%s
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, macLabel, self, extraXML.String(), logPath, logPath)
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

// xmlEscape keeps an argument from breaking the plist it is embedded in.
func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

func winInstall(self string, extra []string) error {
	// ONLOGON task running the agent, recreated (/f) if it exists. Survives the
	// console closing and re-runs after the user logs in following a reboot. The agent
	// Scheduled Tasks do not have a restart-on-exit policy. The internal
	// supervisor keeps the child alive and starts the replaced binary after an
	// update terminates the old child.
	// schtasks takes the whole command as one string, so each argument that could
	// contain a space needs its own quotes.
	tr := fmt.Sprintf(`"%s" __supervise`, self)
	for _, a := range extra {
		tr += ` "` + strings.ReplaceAll(a, `"`, `\"`) + `"`
	}
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

func serviceInstall(self string, extra []string) error {
	switch runtime.GOOS {
	case "linux":
		return linuxInstall(self, extra)
	case "darwin":
		return macInstall(self, extra)
	case "windows":
		return winInstall(self, extra)
	case "android":
		return androidServiceUnsupported(self)
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
	case "android":
		return fmt.Errorf("nothing to uninstall: Android never had a wanctl service (see `wanctl service install`)")
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
	case "android":
		fmt.Println("service: not applicable on Android (no user-installable service manager)")
		fmt.Println("  use `wanctl status` for the detached agent started by `wanctl start`")
		return nil
	}
	return fmt.Errorf("not supported on %s", runtime.GOOS)
}
