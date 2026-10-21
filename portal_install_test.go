package main

import (
	"errors"
	"strings"
	"testing"
)

func TestPortalUpdateDecision(t *testing.T) {
	for _, tc := range []struct {
		name, current, target, platform string
		apk, write                      bool
		restartErr, want                string
	}{
		{"stable", "v0.26.0", "v0.27.0", "linux", false, true, "", ""},
		{"beta", "v0.26.0", "v0.27.0-beta.1", "windows", false, true, "", ""},
		{"promote", "v0.27.0-beta.1", "v0.27.0", "darwin", false, true, "", ""},
		{"same", "v0.26.0", "v0.26.0", "linux", false, true, "", "strictly newer"},
		{"downgrade", "v0.27.0", "v0.26.0", "linux", false, true, "", "strictly newer"},
		{"beta below stable", "v0.27.0", "v0.27.0-beta.2", "linux", false, true, "", "strictly newer"},
		{"dev", "dev", "v0.27.0", "linux", false, true, "", "development"},
		{"empty", "", "v0.27.0", "linux", false, true, "", "development"},
		{"apk", "v0.26.0", "v0.27.0", "android", true, false, "", "app's update check"},
		{"termux", "v0.26.0", "v0.27.0", "android", false, true, "", "cannot restart"},
		{"rights", "v0.26.0", "v0.27.0", "linux", false, false, "", "/system/bin"},
		{"supervisor", "v0.26.0", "v0.27.0", "windows", false, true, "restart refused", "restart refused"},
		{"untrusted", "v0.26.0", "../../bad", "linux", false, true, "", "版本格式"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := portalUpdateProbe{tc.current, "/system/bin/wanctl", tc.platform, func(string) bool { return tc.apk }, func(string) bool { return tc.write }, func() error {
				if tc.restartErr != "" {
					return errors.New(tc.restartErr)
				}
				return nil
			}}
			err := checkPortalUpdate(p, tc.target)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("%v, want %q", err, tc.want)
			}
		})
	}
}

func TestPortalRestartModes(t *testing.T) {
	for _, tc := range []struct {
		name, os                           string
		managed, supervised, zero, allowed bool
	}{
		{"foreground Unix", "linux", false, false, false, true},
		{"detached Unix", "darwin", false, false, false, true},
		{"launchd", "darwin", true, false, false, true},
		{"systemd", "linux", true, false, false, true},
		{"Unix supervisor", "linux", true, true, false, true},
		{"Windows foreground", "windows", false, false, false, true},
		{"Windows detached", "windows", false, false, false, true},
		{"Windows service", "windows", true, false, true, false},
		{"SYSTEM task with loop", "windows", true, true, true, true},
		{"SYSTEM task without loop", "windows", false, false, true, false},
		{"old Windows supervisor", "windows", true, false, false, false},
		{"Windows logon task", "windows", true, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkPortalRestartMode(tc.os, tc.managed, tc.supervised, tc.zero); (err == nil) != tc.allowed {
				t.Fatal(err)
			}
		})
	}
}
