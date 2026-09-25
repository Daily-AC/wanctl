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
	"unicode/utf16"

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
	self, err := selfPath()
	if err != nil {
		return fmt.Errorf("locate wanctl binary: %w", err)
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		self, _ = selfPath()
	}

	switch action {
	case "install":
		if config.EnvOr("WANCTL_TOKEN", config.StoredToken()) == "" {
			return fmt.Errorf("not logged in yet — run `wanctl start` (device) or `wanctl login` first so the service has a token")
		}
		fs := withHelp(flag.NewFlagSet("service install", flag.ContinueOnError))
		name := fs.String("name", "", "device name to bake into the unit (default: hostname, resolved at every start)")
		portalFPs := fs.String("portal-fps", "", "comma-separated portal admin fingerprints the agent must trust")
		mode := fs.String("mode", "", "policy mode to bake in; omit so the persisted mode (and portal switches) win")
		relay := fs.String("relay", "", "relay URL to bake into the unit (default: the currently configured relay)")
		tr := fs.String("transport", "", "transport to bake into the unit: ws or http (default: the currently configured transport)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		relayURL := *relay
		if relayURL == "" {
			var err error
			if relayURL, err = config.Relay(); err != nil {
				return fmt.Errorf("the unit needs a relay URL baked in (service managers don't read your shell profile): %w", err)
			}
		}
		transport := *tr
		if transport == "" {
			transport = config.Transport()
		}
		extra, err := serviceAgentArgs(*name, *portalFPs, *mode, relayURL, transport)
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
// out silently degrades to the hostname, portal fingerprints left out leave
// the device unable to accept portal-side decisions at all, and a relay left
// out kills the agent outright on binaries built without a deployment default
// — service managers don't read the shell profile the user exported
// WANCTL_RELAY into (issue #2).
//
// --mode is deliberately available but not defaulted: baking a mode in makes the
// unit outrank the persisted mode, so a portal-side switch is undone by the next
// restart.
func serviceAgentArgs(name, portalFPs, mode, relay, transport string) ([]string, error) {
	if relay == "" {
		return nil, fmt.Errorf("--relay: the unit needs a relay URL baked in")
	}
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
	extra = append(extra, "--relay", relay)
	if transport != "" {
		if transport != "ws" && transport != "http" {
			return nil, fmt.Errorf("--transport: want ws or http, got %q", transport)
		}
		extra = append(extra, "--transport", transport)
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

// xmlEscape keeps an argument from breaking the plist or task XML it is
// embedded in.
func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

// winTaskXML renders the logon task that runs the agent. It is registered from
// XML because the settings that keep an agent alive have no schtasks flag, and
// the defaults `schtasks /create /sc onlogon` leaves behind each stop it:
//
//   - The action is a console program, so every logon opened a console window,
//     and closing that stray window killed the agent (this took a real device
//     offline). conhost --headless gives the supervisor a console nobody can
//     see or close, while keeping it in the interactive session (a "run whether
//     logged on or not" task would move it to session 0, away from the user's
//     desktop).
//   - DisallowStartIfOnBatteries and StopIfGoingOnBatteries default to true, so
//     a laptop that logs in on battery never starts the agent, and unplugging
//     one stops it.
//   - ExecutionTimeLimit defaults to 72 hours, after which the scheduler ends
//     the task.
//
// The logon trigger names the installing user. /sc onlogon registers an
// any-user trigger, which an unelevated prompt is refused ("Access is denied"),
// so the install used to need an elevated prompt; the agent only ever runs as
// this user anyway. Task Scheduler has no restart-on-exit policy; the
// __supervise loop keeps the agent alive and picks up updated binaries.
func winTaskXML(user, conhost, self string, extra []string) string {
	// The arguments reach wanctl as one command line, so each one that could
	// contain a space needs its own quotes.
	args := fmt.Sprintf(`--headless "%s" __supervise`, self)
	for _, a := range extra {
		args += ` "` + strings.ReplaceAll(a, `"`, `\"`) + `"`
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>wanctl agent (remote device control)</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>%[1]s</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>%[1]s</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>%[2]s</Command>
      <Arguments>%[3]s</Arguments>
    </Exec>
  </Actions>
</Task>
`, xmlEscape(user), xmlEscape(conhost), xmlEscape(args))
}

// utf16File encodes s as UTF-16LE with a byte-order mark, the encoding the
// task XML declares and schtasks /xml reads.
func utf16File(s string) []byte {
	units := utf16.Encode([]rune(s))
	b := make([]byte, 2, 2+2*len(units))
	b[0], b[1] = 0xFF, 0xFE
	for _, u := range units {
		b = append(b, byte(u), byte(u>>8))
	}
	return b
}

func winInstall(self string, extra []string) error {
	u, err := user.Current()
	if err != nil {
		return fmt.Errorf("current user: %w", err)
	}
	sysRoot := os.Getenv("SystemRoot")
	if sysRoot == "" {
		sysRoot = `C:\Windows`
	}
	conhost := filepath.Join(sysRoot, "System32", "conhost.exe")
	f, err := os.CreateTemp("", "wanctl-task-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(utf16File(winTaskXML(u.Username, conhost, self, extra)))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("write task definition: %w", err)
	}
	// Recreated (/f) if it exists. A task an elevated prompt created earlier
	// can only be replaced from an elevated prompt.
	if out, err := run("schtasks", "/create", "/tn", winTaskName, "/xml", f.Name(), "/f"); err != nil {
		return fmt.Errorf("schtasks create: %v\n%s\n(if an existing %s task was installed from an elevated prompt, run this from one too)", err, out, winTaskName)
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
