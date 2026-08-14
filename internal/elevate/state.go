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

// maxADBPortAge is how long a discovered port is trusted. Wireless debugging
// does not survive a reboot and its port changes each time it is enabled, so a
// stale entry is a wrong answer rather than an old one.
const maxADBPortAge = 30 * time.Minute

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
