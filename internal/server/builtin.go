package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"
)

const (
	deviceStateFileEnv = "WANCTL_DEVICE_STATE_FILE"
	maxBatteryStateAge = 10 * time.Minute
)

type batteryState struct {
	Level        *int     `json:"level"`
	Status       string   `json:"status"`
	Plugged      string   `json:"plugged"`
	TemperatureC *float64 `json:"temperature_c"`
	Health       string   `json:"health"`
	UpdatedAt    string   `json:"updated_at"`
}

type batteryOutput struct {
	Level        int     `json:"level"`
	Status       string  `json:"status"`
	Plugged      string  `json:"plugged"`
	TemperatureC float64 `json:"temperature_c"`
	Health       string  `json:"health"`
	UpdatedAt    string  `json:"updated_at"`
	AgeSeconds   int64   `json:"age_seconds"`
}

// RunBuiltin handles device-native verbs before command source reaches a shell.
func RunBuiltin(command string, out io.Writer) (bool, int, error) {
	handled, data, err := runBuiltin(
		command, runtime.GOOS, os.Getenv(deviceStateFileEnv), time.Now(), os.ReadFile)
	if !handled || err != nil {
		return handled, 0, err
	}
	_, err = out.Write(data)
	return true, 0, err
}

func runBuiltin(command, goos, statePath string, now time.Time, readFile func(string) ([]byte, error)) (bool, []byte, error) {
	if strings.TrimSpace(command) != "battery" {
		return false, nil, nil
	}
	if goos != "android" {
		return true, nil, errors.New("battery is only available on Android APK agents")
	}
	if statePath == "" {
		return true, nil, errors.New("battery state unavailable: Android app state collector is not configured")
	}
	data, err := readFile(statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil, errors.New("battery state unavailable: state file does not exist; Android app collector may not be running")
		}
		return true, nil, fmt.Errorf("battery state unavailable: read state file: %w", err)
	}
	output, err := formatBatteryState(data, now, maxBatteryStateAge)
	if err != nil {
		return true, nil, fmt.Errorf("battery state unavailable: %w", err)
	}
	return true, append(output, '\n'), nil
}

// formatBatteryState validates a Java collector snapshot, checks its age, and
// produces the stable public JSON shape without consulting process state.
func formatBatteryState(data []byte, now time.Time, maxAge time.Duration) ([]byte, error) {
	var state batteryState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("invalid state file: %w", err)
	}
	if state.Level == nil || state.TemperatureC == nil || state.Status == "" ||
		state.Plugged == "" || state.Health == "" || state.UpdatedAt == "" {
		return nil, errors.New("invalid state file: missing required battery field")
	}
	if *state.Level < 0 || *state.Level > 100 {
		return nil, fmt.Errorf("invalid state file: level %d is outside 0..100", *state.Level)
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, state.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("invalid state file: updated_at: %w", err)
	}
	age := now.Sub(updatedAt)
	if age < 0 {
		return nil, fmt.Errorf("invalid state file: updated_at is %d seconds in the future", -age/time.Second)
	}
	ageSeconds := int64(age / time.Second)
	if age > maxAge {
		return nil, fmt.Errorf("state is stale (updated %d seconds ago); Android app collector may not be running", ageSeconds)
	}

	return json.Marshal(batteryOutput{
		Level:        *state.Level,
		Status:       state.Status,
		Plugged:      state.Plugged,
		TemperatureC: *state.TemperatureC,
		Health:       state.Health,
		UpdatedAt:    state.UpdatedAt,
		AgeSeconds:   ageSeconds,
	})
}
