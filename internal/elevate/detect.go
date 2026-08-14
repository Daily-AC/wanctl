package elevate

import (
	"runtime"
	"strings"
)

// SwitchEnv is how the device says elevation is allowed at all. The APK writes
// it when its 提权通道 switch is on; the Termux and adb-shell routes can export
// it by hand. Absent or falsey means every elevated command is refused with an
// explanation, which is the default and stays the default through an upgrade —
// no device becomes more powerful because someone installed a new version.
const SwitchEnv = "WANCTL_ELEVATION"

// Configure builds the manager for this platform and environment.
//
// Elevation is Android-only on purpose. A `su` channel would work on Linux, but
// wanctl's Linux agents are ordinary user services on machines with real
// service managers and real shells; adding a privilege-escalation path there
// would widen the attack surface of every existing deployment to solve a
// problem only Android has. See ADR 0004.
func Configure(goos, configDir string, getenv func(string) string) *Manager {
	if goos != "android" {
		return NewManager(false, "elevation channels exist only on Android; this device is "+goos)
	}
	channels := []Channel{NewSu(), NewADB(configDir, adbKeyName(getenv))}
	if !truthy(getenv(SwitchEnv)) {
		// Registered even while switched off, so `status` can answer "which
		// channels does this build have, and why is none of them usable" with
		// the switch as the reason. Nothing is probed in this state — see
		// Manager.Statuses — so a rooted device raises no consent dialog for a
		// feature its owner has not turned on.
		return NewManager(false, "turn on 提权通道 in the wanctl app (or set "+SwitchEnv+"=1)", channels...)
	}
	return NewManager(true, "", channels...)
}

// ConfigureDefault builds the manager for the running process.
func ConfigureDefault(configDir string, getenv func(string) string) *Manager {
	return Configure(runtime.GOOS, configDir, getenv)
}

// adbKeyName is what the device's "Allow debugging?" prompt shows and what ends
// up in its adb_keys file, so it has to identify wanctl rather than look like
// any other host that ever connected.
func adbKeyName(getenv func(string) string) string {
	name := strings.TrimSpace(getenv("WANCTL_DEVICE_NAME"))
	if name == "" {
		return "wanctl-agent"
	}
	return "wanctl@" + name
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
