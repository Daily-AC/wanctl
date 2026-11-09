package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func launchdTestSnapshot(t *testing.T) launchdSnapshot {
	t.Helper()
	self := filepath.Join(t.TempDir(), "wanctl")
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return launchdSnapshot{
		domain:      "gui/501",
		label:       macLabel,
		plistLabel:  macLabel,
		state:       "spawn scheduled",
		lastExit:    "78: EX_CONFIG",
		properties:  "keepalive | runatload | needs LWCR update | managed LWCR | has LWCR",
		plistPath:   filepath.Join(filepath.Dir(self), "com.wanctl.agent.plist"),
		programPath: self,
		plistExists: true,
	}
}

func TestLaunchdRecoveryEligibility(t *testing.T) {
	oldOS := managedRestartGOOS
	managedRestartGOOS = "darwin"
	t.Cleanup(func() { managedRestartGOOS = oldOS })

	base := launchdTestSnapshot(t)
	cases := []struct {
		name string
		edit func(*launchdSnapshot)
		want bool
	}{
		{name: "all conditions", want: true},
		{name: "only exit 78", edit: func(s *launchdSnapshot) { s.properties = "keepalive | managed LWCR" }, want: false},
		{name: "label mismatch", edit: func(s *launchdSnapshot) { s.label = "com.other.agent" }, want: false},
		{name: "path mismatch", edit: func(s *launchdSnapshot) { s.programPath = filepath.Join(filepath.Dir(s.programPath), "other") }, want: false},
		{name: "plist missing", edit: func(s *launchdSnapshot) { s.plistExists = false }, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			if tc.edit != nil {
				tc.edit(&s)
			}
			if got := launchdRecoveryEligible(s, base.programPath); got != tc.want {
				t.Fatalf("eligible = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLaunchdRecoveryTransaction(t *testing.T) {
	old := launchctlRun
	t.Cleanup(func() { launchctlRun = old })

	t.Run("success and successor appears", func(t *testing.T) {
		s := launchdTestSnapshot(t)
		var calls [][]string
		launchctlRun = func(args ...string) (string, error) {
			calls = append(calls, append([]string(nil), args...))
			return "", nil
		}
		got, err := recoverLaunchdAndAwait(s, func() managedRestartResult { return managedRestartReplaced })
		if err != nil || got != managedRestartReplaced {
			t.Fatalf("result = %v, err = %v", got, err)
		}
		want := [][]string{{"bootout", "gui/501/com.wanctl.agent"}, {"bootstrap", "gui/501", s.plistPath}}
		if !reflect.DeepEqual(calls, want) {
			t.Fatalf("launchctl calls = %#v, want %#v", calls, want)
		}
	})

	t.Run("success but successor still fails", func(t *testing.T) {
		s := launchdTestSnapshot(t)
		calls := 0
		launchctlRun = func(args ...string) (string, error) { calls++; return "", nil }
		got, err := recoverLaunchdAndAwait(s, func() managedRestartResult { return managedRestartStopped })
		if err != nil || got != managedRestartStopped {
			t.Fatalf("result = %v, err = %v", got, err)
		}
		if calls != 2 {
			t.Fatalf("launchctl calls = %d, want 2", calls)
		}
	})

	t.Run("bootout failure stops transaction", func(t *testing.T) {
		s := launchdTestSnapshot(t)
		bootouts, bootstraps := 0, 0
		launchctlRun = func(args ...string) (string, error) {
			switch args[0] {
			case "bootout":
				bootouts++
				return "busy", errors.New("busy")
			case "bootstrap":
				bootstraps++
			}
			return "", nil
		}
		if _, err := recoverLaunchdAndAwait(s, func() managedRestartResult { return managedRestartReplaced }); err == nil {
			t.Fatal("recovery succeeded after bootout failure")
		}
		if bootouts != 1 || bootstraps != 0 {
			t.Fatalf("bootout=%d bootstrap=%d, want 1/0", bootouts, bootstraps)
		}
	})
}

func TestLaunchdDiagnosticsAreCopyable(t *testing.T) {
	s := launchdTestSnapshot(t)
	if got := launchdDiagnostic(s); !strings.Contains(got, "state=spawn scheduled") || !strings.Contains(got, "last exit code=78: EX_CONFIG") || !strings.Contains(got, "properties=keepalive") {
		t.Fatalf("diagnostic = %q", got)
	}
	if got := launchdRecoveryCommand(s); !strings.Contains(got, "bootout gui/501/com.wanctl.agent") || !strings.Contains(got, "bootstrap gui/501") {
		t.Fatalf("command = %q", got)
	}
}

func TestParseLaunchdPlist(t *testing.T) {
	program := filepath.Join(t.TempDir(), "wanctl")
	p := parseLaunchdPlist([]byte(fmt.Sprintf(`<?xml version="1.0"?><plist><dict>
<key>Label</key><string>com.wanctl.agent</string>
<key>ProgramArguments</key><array><string>%s</string><string>agent</string><string>--managed</string></array>
</dict></plist>`, program)))
	if p.label != macLabel || p.programPath != program {
		t.Fatalf("parsed plist = %#v", p)
	}
}

func TestVerifyManagedRestartVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as the fake binary")
	}
	self := filepath.Join(t.TempDir(), "wanctl")
	if err := os.WriteFile(self, []byte("#!/bin/sh\necho v1.2.3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := verifyManagedRestartVersion(self, []string{"v1.2.3"}); err != nil {
		t.Errorf("matching version: %v", err)
	}
	if err := verifyManagedRestartVersion(self, nil); err != nil {
		t.Errorf("no expected version: %v", err)
	}
	err := verifyManagedRestartVersion(self, []string{"v9.9.9"})
	if err == nil || !strings.Contains(err.Error(), "v1.2.3") || !strings.Contains(err.Error(), "v9.9.9") {
		t.Errorf("mismatched version: %v", err)
	}
}
