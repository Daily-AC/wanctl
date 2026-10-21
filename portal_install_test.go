package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"wanctl/internal/config"
)

func TestPortalUpdateDecision(t *testing.T) {
	for _, tc := range []struct {
		name, current, target, platform string
		apk, write                      bool
		want                            string
	}{
		{"stable", "v0.26.0", "v0.27.0", "linux", false, true, ""},
		{"beta", "v0.26.0", "v0.27.0-beta.1", "windows", false, true, ""},
		{"promote", "v0.27.0-beta.1", "v0.27.0", "darwin", false, true, ""},
		{"same", "v0.26.0", "v0.26.0", "linux", false, true, "strictly newer"},
		{"downgrade", "v0.27.0", "v0.26.0", "linux", false, true, "strictly newer"},
		{"beta below stable", "v0.27.0", "v0.27.0-beta.2", "linux", false, true, "strictly newer"},
		{"dev", "dev", "v0.27.0", "linux", false, true, "development"},
		{"empty", "", "v0.27.0", "linux", false, true, "development"},
		{"apk", "v0.26.0", "v0.27.0", "android", true, false, "app's update check"},
		{"termux", "v0.26.0", "v0.27.0", "android", false, true, "cannot restart"},
		{"rights", "v0.26.0", "v0.27.0", "linux", false, false, "/system/bin"},
		{"untrusted", "v0.26.0", "../../bad", "linux", false, true, "版本格式"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := portalUpdateProbe{tc.current, "/system/bin/wanctl", tc.platform, func(string) bool { return tc.apk }, func(string) bool { return tc.write }}
			err := checkPortalUpdate(p, tc.target)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("%v, want %q", err, tc.want)
			}
		})
	}
}

// The web request ignores auto_update's setting, but uses its platform/build
// eligibility and the existing restart path, without inspecting the parent.
func TestPortalUpdateUsesAutoUpdateEligibility(t *testing.T) {
	for _, platform := range []string{"windows", "darwin", "linux"} {
		for _, managed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/managed=%v", platform, managed), func(t *testing.T) {
				t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
				if managed {
					if err := config.WriteManagedPID(os.Getpid()); err != nil {
						t.Fatal(err)
					}
				}
				writable := func(string) bool { return true }
				apk := func(string) bool { return false }
				auto := decideAutoUpdate(t.Context(), autoUpdateProbe{enabled: func() bool { return true }, version: "v0.26.0", self: "/bin/wanctl", isAPK: apk, writable: writable, busy: func() bool { return false }, offered: func(context.Context) (string, error) { return "v0.27.0", nil }})
				err := checkPortalUpdate(portalUpdateProbe{"v0.26.0", "/bin/wanctl", platform, apk, writable}, "v0.27.0")
				if auto.action != autoUpdateReady || err != nil {
					t.Fatalf("auto=%v web=%v", auto.action, err)
				}
			})
		}
	}
}
