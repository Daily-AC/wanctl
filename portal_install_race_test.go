package main

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"wanctl/internal/agent"
	"wanctl/internal/client"
	"wanctl/internal/protocol"
	"wanctl/internal/relay"
	"wanctl/internal/release"
	"wanctl/internal/transport"
)

// Port of TestInvariant3AutoUpdateRace. It retains real signed downloads and
// binary replacement, and uses a real authenticated console instead of the
// review copy's production-only bridge to the private beginInstall method.
func TestInvariant3AutoUpdateRace(t *testing.T) {
	for _, path := range []string{"auto", "CLI"} {
		t.Run(path, func(t *testing.T) {
			newer := signedUpdateServer(t, []byte("v3.0.0"), runtime.GOOS, runtime.GOARCH, "v3.0.0", nil)
			defer newer.Close()
			newerKey := release.TrustedPublicKeys
			older := signedUpdateServer(t, []byte("v2.0.0"), runtime.GOOS, runtime.GOARCH, "v2.0.0", nil)
			defer older.Close()
			release.TrustedPublicKeys += "," + newerKey
			a, self, conn := raceInstallAgent(t)
			web := configurePortalUpdate(a, "v1.0.0", func() {})
			t.Setenv("WANCTL_RELEASE_BASE", newer.URL+"/dl")
			t.Setenv("WANCTL_RELAY", newer.URL)
			if path == "auto" {
				u := newAutoUpdater(self, "v1.0.0", a.Busy, func() {})
				v, err := a.InstallAutoUpdate(t.Context(), func(ctx context.Context, current string) (string, error) {
					u.probe.version = current
					return u.installLatest(ctx)
				})
				if err != nil || v != "v3.0.0" {
					t.Fatalf("auto: %s %v", v, err)
				}
			} else {
				// cmdUpdate is normally a separate process when invoked through exec.
				// --no-restart reproduces the post-swap/pre-restart scheduling boundary.
				if err := cmdUpdate(t.Context(), []string{"--no-restart", "v3.0.0"}); err != nil {
					t.Fatal(err)
				}
			}
			// Pause after releasing the transaction lock, before tick/CLI restarts.
			t.Setenv("WANCTL_RELEASE_BASE", older.URL+"/dl")
			t.Setenv("WANCTL_RELAY", older.URL)
			for _, target := range []string{"v2.0.0", "v4.0.0"} {
				if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindPackageInstall, Package: "wanctl", Version: target}); err != nil {
					t.Fatal(err)
				}
				for {
					m, err := protocol.ReadMessage(conn)
					if err != nil {
						t.Fatal(err)
					}
					if m.Kind == protocol.KindApprovalNotif {
						continue
					}
					if m.Kind != protocol.KindError || m.Reason != agent.ErrRestartPending.Error() {
						t.Fatalf("accepted after swap: %+v", m)
					}
					break
				}
			}
			got, err := os.ReadFile(self)
			if err != nil || string(got) != "v3.0.0" || web.updated.Load() {
				t.Fatalf("disk=%q web.updated=%v err=%v", got, web.updated.Load(), err)
			}
		})
	}
}

func raceInstallAgent(t *testing.T) (*agent.Agent, string, net.Conn) {
	t.Helper()
	r := relay.New(relay.EnvTokenStore("device:alice,portal:portal"))
	r.SetPortalNS("portal")
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	base := "ws" + strings.TrimPrefix(srv.URL, "http")
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	id, err := transport.LoadOrCreateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	known, err := transport.OpenStore("known_servers.json")
	if err != nil {
		t.Fatal(err)
	}
	controller := client.NewWith(id, known, base, "portal", "ws")
	observer := client.NewWith(id, known, base, "device", "ws")
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	a, err := agent.New(agent.Options{RelayURL: base, Token: "device", Name: "install-race", PortalFP: id.Fingerprint, Version: "v1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	self := filepath.Join(t.TempDir(), "wanctl")
	if err := os.WriteFile(self, []byte("v1.0.0"), 0755); err != nil {
		t.Fatal(err)
	}
	old := termuxSelf
	termuxSelf = self
	t.Cleanup(func() { termuxSelf = old })
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() { cancel(); a.Close(); <-done })
	for {
		devs, _ := observer.Peers(ctx)
		if len(devs) > 0 {
			break
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
	target := "alice/" + a.DeviceID()
	_, _, err = controller.Pair(ctx, target)
	var trust *client.TrustRequiredError
	if !errors.As(err, &trust) {
		t.Fatalf("trust: %v", err)
	}
	if _, err = controller.PinServer(ctx, trust.Target, trust.Fingerprint, false); err != nil {
		t.Fatal(err)
	}
	conn, err := controller.OpenConsole(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	return a, self, conn
}
