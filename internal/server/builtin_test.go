package server

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

var batteryNow = time.Date(2026, 8, 13, 18, 42, 15, 0, time.UTC)

const validBatteryState = `{"level":76,"status":"charging","plugged":"usb","temperature_c":31.4,"health":"good","updated_at":"2026-08-13T18:42:03Z"}`

func TestFormatBatteryState(t *testing.T) {
	got, err := formatBatteryState([]byte(validBatteryState), batteryNow, maxBatteryStateAge)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"level":76,"status":"charging","plugged":"usb","temperature_c":31.4,"health":"good","updated_at":"2026-08-13T18:42:03Z","age_seconds":12}`
	if string(got) != want {
		t.Fatalf("output = %s, want %s", got, want)
	}
}

func TestRunBuiltinFormatsBattery(t *testing.T) {
	handled, got, err := runBuiltin(" battery \n", "android", "/state/device.json", batteryNow, func(path string) ([]byte, error) {
		if path != "/state/device.json" {
			t.Fatalf("read path = %q", path)
		}
		return []byte(validBatteryState), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"level":76,"status":"charging","plugged":"usb","temperature_c":31.4,"health":"good","updated_at":"2026-08-13T18:42:03Z","age_seconds":12}` + "\n"
	if !handled || string(got) != want {
		t.Fatalf("handled=%v output=%q, want %q", handled, got, want)
	}
}

func TestFormatBatteryStateRejectsDamagedFile(t *testing.T) {
	for _, data := range []string{`{"level":`, `{"level":76}`} {
		if _, err := formatBatteryState([]byte(data), batteryNow, maxBatteryStateAge); err == nil || !strings.Contains(err.Error(), "invalid state file") {
			t.Fatalf("formatBatteryState(%q) error = %v, want invalid state file", data, err)
		}
	}
}

func TestFormatBatteryStateRejectsStaleFile(t *testing.T) {
	updatedAt := time.Date(2026, 8, 13, 18, 42, 3, 0, time.UTC)
	now := updatedAt.Add(maxBatteryStateAge)
	if _, err := formatBatteryState([]byte(validBatteryState), now, maxBatteryStateAge); err != nil {
		t.Fatalf("state at the freshness boundary was rejected: %v", err)
	}
	now = now.Add(time.Second)
	if _, err := formatBatteryState([]byte(validBatteryState), now, maxBatteryStateAge); err == nil || !strings.Contains(err.Error(), "state is stale") {
		t.Fatalf("stale state error = %v", err)
	}
}

func TestRunBuiltinReportsMissingStateFile(t *testing.T) {
	handled, _, err := runBuiltin("battery", "android", "/missing/device.json", batteryNow, func(string) ([]byte, error) {
		return nil, os.ErrNotExist
	})
	if !handled || err == nil || !strings.Contains(err.Error(), "state file does not exist") {
		t.Fatalf("handled=%v error=%v", handled, err)
	}
}

func TestRunBuiltinRejectsBatteryOffAndroid(t *testing.T) {
	readCalled := false
	handled, _, err := runBuiltin("battery", "linux", "/state/device.json", batteryNow, func(string) ([]byte, error) {
		readCalled = true
		return nil, errors.New("unexpected read")
	})
	if !handled || err == nil || !strings.Contains(err.Error(), "only available on Android") {
		t.Fatalf("handled=%v error=%v", handled, err)
	}
	if readCalled {
		t.Fatal("non-Android battery verb read the Android state file")
	}
}

func TestRunBuiltinLeavesShellCommandsAlone(t *testing.T) {
	handled, _, err := runBuiltin("battery --verbose", "android", "", batteryNow, nil)
	if handled || err != nil {
		t.Fatalf("handled=%v error=%v", handled, err)
	}
}
