package agent

import (
	"context"
	"errors"
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
	a.ConfigureInstall(InstallHooks{Check: func(string, string) error { return nil }, Run: func(_ context.Context, current, v string, stage func(string)) (string, error) {
		stage("installing")
		return v, nil
	}})
	req := protocol.Message{Kind: protocol.KindPackageInstall, Package: "wanctl", Version: "v0.27.0"}
	resume := a.desktop.PauseForUpdate()
	busy, _ := a.beginInstall(req, func(protocol.InstallProgress) {})
	resume()
	if !strings.Contains(busy.Reason, "busy") {
		t.Fatalf("%+v", busy)
	}
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

func TestInstallFailureKeepsDiagnosticsOnlyInEventLog(t *testing.T) {
	a := newTestAgent(t)
	logger, err := eventlog.Open("install-failure.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	a.log = logger
	const diagnostic = `fetch https://github.com/Daily-AC/wanctl/releases/download/v0.29.0-beta.9/manifest.json: Get "https://github.com/example": net/http: TLS handshake timeout`
	const reason = "the device could not reach github.com"
	a.ConfigureInstall(InstallHooks{Check: func(string, string) error { return nil }, Run: func(context.Context, string, string, func(string)) (string, error) {
		return "", &InstallError{Reason: reason, Err: errors.New(diagnostic)}
	}})
	var last protocol.InstallProgress
	response, work := a.beginInstall(protocol.Message{Kind: protocol.KindPackageInstall, Package: "wanctl", Version: "v0.29.0-beta.9"}, func(p protocol.InstallProgress) { last = p })
	if response.Kind == protocol.KindError || work == nil {
		t.Fatal(response)
	}
	work()
	if last.Stage != "failed" || last.Reason != reason {
		t.Fatalf("wire: %+v", last)
	}
	events, err := logger.Read(eventlog.Filter{Type: "install"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || !strings.Contains(events[1].Detail, diagnostic) {
		t.Fatalf("lost diagnostic: %+v", events)
	}
}

func TestInstallRestartPendingIsIrreversible(t *testing.T) {
	for _, path := range []string{"auto", "web"} {
		t.Run(path, func(t *testing.T) {
			a := newTestAgent(t)
			a.ConfigureInstall(InstallHooks{Version: "v1.0.0", Check: func(current, target string) error {
				if current != "v1.0.0" {
					t.Fatalf("current=%s", current)
				}
				return nil
			}, Run: func(_ context.Context, current, target string, _ func(string)) (string, error) { return target, nil }})
			if path == "auto" {
				_, err := a.InstallAutoUpdate(t.Context(), func(_ context.Context, current string) (string, error) {
					if current != "v1.0.0" {
						t.Fatal(current)
					}
					return "v3.0.0", nil
				})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				_, work := a.beginInstall(protocol.Message{Package: "wanctl", Version: "v3.0.0"}, func(protocol.InstallProgress) {})
				if work == nil {
					t.Fatal("refused first install")
				}
				work()
			}
			if !a.restartPending || a.installVersion != "v3.0.0" {
				t.Fatalf("pending=%v installed=%s", a.restartPending, a.installVersion)
			}
			for _, req := range []protocol.Message{{Package: "wanctl", Version: "v2.0.0"}, {Package: "wanctl", Version: "v4.0.0"}, {Package: "opencu", Version: "latest"}} {
				reply, work := a.beginInstall(req, func(protocol.InstallProgress) { t.Fatal("unexpected progress") })
				if work != nil || reply.Reason != ErrRestartPending.Error() {
					t.Fatalf("%+v", reply)
				}
			}
			_, err := a.InstallAutoUpdate(t.Context(), func(context.Context, string) (string, error) { t.Fatal("auto-update ran after swap"); return "", nil })
			if !errors.Is(err, ErrRestartPending) {
				t.Fatal(err)
			}
		})
	}
}

func TestFailedAutoInstallReleasesReservation(t *testing.T) {
	a := newTestAgent(t)
	a.ConfigureInstall(InstallHooks{Version: "v1.0.0", Check: func(string, string) error { return nil }, Run: func(context.Context, string, string, func(string)) (string, error) { return "v2.0.0", nil }})
	_, err := a.InstallAutoUpdate(t.Context(), func(context.Context, string) (string, error) { return "", errors.New("download failed") })
	if err == nil || a.restartPending {
		t.Fatalf("%v pending=%v", err, a.restartPending)
	}
	_, work := a.beginInstall(protocol.Message{Package: "wanctl", Version: "v2.0.0"}, func(protocol.InstallProgress) {})
	if work == nil {
		t.Fatal("failure left device locked")
	}
	work()
}

func TestWebInstallPendingBeforeRestartCallback(t *testing.T) {
	a := newTestAgent(t)
	called := false
	a.ConfigureInstall(InstallHooks{Version: "v1.0.0", Check: func(string, string) error { return nil }, Run: func(context.Context, string, string, func(string)) (string, error) { return "v2.0.0", nil }, Restart: func() {
		called = true
		resp, work := a.beginInstall(protocol.Message{Package: "wanctl", Version: "v3.0.0"}, func(protocol.InstallProgress) {})
		if work != nil || resp.Reason != ErrRestartPending.Error() {
			t.Fatalf("during restart: %+v", resp)
		}
	}})
	_, work := a.beginInstall(protocol.Message{Package: "wanctl", Version: "v2.0.0"}, func(protocol.InstallProgress) {})
	if work == nil {
		t.Fatal("initial install refused")
	}
	work()
	if !called {
		t.Fatal("no handover")
	}
}
