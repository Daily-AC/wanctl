package agent

import (
	"context"
	"strings"
	"testing"

	"wanctl/internal/eventlog"
	"wanctl/internal/protocol"
)

func TestInstallValidation(t *testing.T) {
	for _, tc := range []struct {
		pkg, version, os string
		ok               bool
	}{
		{"wanctl", "v0.27.0", "linux", true}, {"wanctl", "v0.27.0-beta.1", "windows", true}, {"wanctl", "v0.27.0;exit", "windows", false},
		{"wanctl", "latest", "linux", false}, {"other", "v1.0.0", "windows", false}, {"opencu", "latest", "windows", true}, {"opencu", "v1.0.0", "windows", false}, {"opencu", "latest", "linux", false},
	} {
		if err := validateInstall(tc.pkg, tc.version, tc.os); (err == nil) != tc.ok {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
}
func TestInstallConcurrentBusyAndAudit(t *testing.T) {
	a := newTestAgent(t)
	log, err := eventlog.Open("install-events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	a.log = log
	var stages []string
	a.ConfigureInstall(InstallHooks{Check: func(string) error { return nil }, Run: func(_ context.Context, v string, stage func(string)) (string, error) {
		stage("installing")
		return v, nil
	}})
	req := protocol.Message{Kind: protocol.KindPackageInstall, Package: "wanctl", Version: "v0.27.0"}
	response, work := a.beginInstall(req, func(p protocol.InstallProgress) { stages = append(stages, p.Stage) })
	if response.Kind == protocol.KindError || work == nil {
		t.Fatalf("%+v", response)
	}
	second, _ := a.beginInstall(req, func(protocol.InstallProgress) {})
	if second.Reason != "an install is already running" {
		t.Fatalf("%+v", second)
	}
	work()
	if strings.Join(stages, ",") != "downloading,installing,installing" {
		t.Fatal(stages)
	}
	resume := a.desktop.PauseForUpdate()
	busy, _ := a.beginInstall(req, func(protocol.InstallProgress) {})
	resume()
	if !strings.Contains(busy.Reason, "busy") {
		t.Fatalf("%+v", busy)
	}
	events, err := a.log.Read(eventlog.Filter{Type: "install"})
	if err != nil || len(events) != 6 {
		t.Fatalf("%+v %v", events, err)
	}
}

// This exercises the unchanged fallback older agents execute, rather than a
// guessed error fixture. The portal wire test consumes the same JSON string.
func TestUnknownConsoleKindLegacyInstallResponse(t *testing.T) {
	a := newTestAgent(t)
	m := a.handleConsoleRPC(protocol.Message{Kind: "future_install_kind"})
	if m.Kind != protocol.KindError || string(m.Data) != `"unknown RPC kind"` {
		t.Fatalf("%+v", m)
	}
}
