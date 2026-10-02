package elevate

import (
	"encoding/json"
	"os"
	"time"
)

// deviceState is the part of the Android app's state file this package reads.
// The file is the same one v0.1.12 introduced for battery; the fields it does
// not know about are ignored, in both directions.
type deviceState struct {
	ADB *adbState `json:"adb"`
}

type adbState struct {
	// Port is where the app's NsdManager found _adb-tls-connect._tcp. Android
	// picks a new one every time wireless debugging is enabled, and receiving
	// mDNS requires a MulticastLock, which is a framework call — so the Java
	// side discovers it and the Go child reads it here.
	Port int `json:"port"`
	// UpdatedAt lets a stale port be ignored rather than dialed. A port from a
	// previous session is not merely useless: something else may be listening
	// on it by now.
	UpdatedAt string `json:"updated_at"`
}

// maxADBPortAge is a backstop, not the thing that retires a port. Clearing the
// port is the Java lifecycle's job: a new service writes the state file with no
// port the moment it has a battery reading, the watch publishes 0 when adbd
// stops advertising, and switching 提权通道 off publishes 0 as well.
//
// The app stamps a port once, when mDNS first reports it — NsdManager does not
// report a service again while it stays up — and rewrites the file on every
// battery broadcast without touching that stamp. An age limit therefore
// measures time since discovery, not whether the port still works, and the 30
// minutes this used to be quietly retired a live port: the first reconnect
// after that (a ROM that drops the socket on screen lock, an agent restarted by
// a settings change) fell back to 5555 and failed on a phone whose wireless
// debugging was on. What the limit still guards against is a file nothing
// maintains any more, and a day is plenty for that.
const maxADBPortAge = 24 * time.Hour

// DiscoveredPort is the wireless-debugging port the Android app last found, or
// 0. The agent watches it to re-probe the adb channel the moment the owner
// turns wireless debugging on or off.
func DiscoveredPort() int { return portFromState(os.Getenv(StateEnv)) }

// portFromState reads the app-discovered wireless-debugging port, or 0.
func portFromState(path string) int {
	if path == "" {
		return 0
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var st deviceState
	if err := json.Unmarshal(data, &st); err != nil || st.ADB == nil {
		return 0
	}
	if st.ADB.Port <= 0 || st.ADB.Port > 65535 {
		return 0
	}
	if st.ADB.UpdatedAt != "" {
		t, err := time.Parse(time.RFC3339Nano, st.ADB.UpdatedAt)
		if err != nil || time.Since(t) > maxADBPortAge {
			return 0
		}
	}
	return st.ADB.Port
}
