package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"wanctl/internal/transport"
)

// fileIn returns <configdir>/name, creating the config dir if needed.
func fileIn(name string) (string, error) {
	dir, err := transport.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// TokenPath is where the OAuth-enrolled agent token is stored.
func TokenPath() (string, error) { return fileIn("token") }

// StoredToken returns the token saved by `wanctl up`, or "" if none. Errors are
// swallowed (treated as "no token") so it composes cleanly as an EnvOr default.
func StoredToken() string {
	p, err := TokenPath()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// SaveToken writes the token to the config dir with owner-only permissions.
func SaveToken(token string) error {
	p, err := TokenPath()
	if err != nil {
		return err
	}
	return os.WriteFile(p, []byte(strings.TrimSpace(token)+"\n"), 0o600)
}

// ClearToken removes the stored token (logout). Missing file is not an error.
func ClearToken() error {
	p, err := TokenPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// NetModePath is where the controller-side network mode is persisted.
func NetModePath() (string, error) { return fileIn("netmode") }

// StoredNetMode returns the persisted controller network mode: "wan" (default),
// "lan" (force intranet relay) or "auto" (probe the intranet relay, fall back
// to wan). Unknown/missing values read as "wan".
func StoredNetMode() string {
	p, err := NetModePath()
	if err != nil {
		return "wan"
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "wan"
	}
	switch m := strings.TrimSpace(string(b)); m {
	case "lan", "auto", "wan":
		return m
	}
	return "wan"
}

// SaveNetMode persists the controller network mode.
func SaveNetMode(mode string) error {
	p, err := NetModePath()
	if err != nil {
		return err
	}
	return os.WriteFile(p, []byte(strings.TrimSpace(mode)+"\n"), 0o600)
}

// LanRelay resolves the intranet relay URL: WANCTL_LAN_RELAY overrides the
// compile-time default.
func LanRelay() string { return EnvOr("WANCTL_LAN_RELAY", DefaultLanRelay) }

// lanPath is the device-side switch for the LAN relay uplink.
func lanPath() (string, error) { return fileIn("lan") }

// LanUplinkEnabled reports the persisted device-side LAN-uplink switch.
// Defaults to true: dialing the intranet relay from outside just fails quietly
// and retries with backoff, and E2E trust does not depend on relay identity.
func LanUplinkEnabled() bool {
	p, err := lanPath()
	if err != nil {
		return true
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return true
	}
	return strings.TrimSpace(string(b)) != "off"
}

// SaveLanUplink persists the device-side LAN-uplink switch.
func SaveLanUplink(on bool) error {
	p, err := lanPath()
	if err != nil {
		return err
	}
	v := "on"
	if !on {
		v = "off"
	}
	return os.WriteFile(p, []byte(v+"\n"), 0o600)
}

// PIDPath / LogPath locate the background agent's pid and log files.
func PIDPath() (string, error) { return fileIn("agent.pid") }
func LogPath() (string, error) { return fileIn("agent.log") }

// ReadPID returns the recorded background-agent pid, or 0 if none recorded.
func ReadPID() int {
	p, err := PIDPath()
	if err != nil {
		return 0
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

// WritePID records the background-agent pid.
func WritePID(pid int) error {
	p, err := PIDPath()
	if err != nil {
		return err
	}
	return os.WriteFile(p, []byte(strconv.Itoa(pid)+"\n"), 0o600)
}

// RemovePID clears the recorded pid (on stop). Missing file is not an error.
func RemovePID() error {
	p, err := PIDPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
