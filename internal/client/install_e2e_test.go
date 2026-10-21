package client

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wanctl/internal/agent"
	"wanctl/internal/protocol"
	"wanctl/internal/relay"
	"wanctl/internal/transport"
)

// Real relay + mutual TLS + current agent dispatcher, on both relay carriers.
// The swap is injected: running the test must never replace the test executable.
func TestPackageInstallThroughRelay(t *testing.T) {
	for _, carrier := range []string{"ws", "http"} {
		t.Run(carrier, func(t *testing.T) {
			r := relay.New(relay.EnvTokenStore("device:alice,portal:portal"))
			r.SetPortalNS("portal")
			srv := httptest.NewServer(r.Handler())
			defer srv.Close()
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
			controller := NewWith(id, known, base, "portal", carrier)
			observer := NewWith(id, known, base, "device", carrier)
			t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
			a, err := agent.New(agent.Options{RelayURL: base, Token: "device", Name: "install-device", Transport: carrier, PortalFP: id.Fingerprint, Version: "v0.26.0"})
			if err != nil {
				t.Fatal(err)
			}
			a.ConfigureInstall(agent.InstallHooks{Check: func(string) error { return nil }, Run: func(_ context.Context, v string, stage func(string)) (string, error) {
				stage("installing")
				return v, nil
			}})
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			done := make(chan error, 1)
			go func() { done <- a.Run(ctx) }()
			defer func() { cancel(); a.Close(); <-done }()
			for {
				devs, _ := observer.Peers(ctx)
				if len(devs) > 0 {
					break
				}
				if ctx.Err() != nil {
					t.Fatal(ctx.Err())
				}
				time.Sleep(10 * time.Millisecond)
			}
			target := "alice/" + a.DeviceID()
			trustServer(t, controller, target)
			conn, err := controller.OpenConsole(ctx, target)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			if err = protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindPackageInstall, Package: "wanctl", Version: "v0.27.0-beta.1"}); err != nil {
				t.Fatal(err)
			}
			ack := false
			var stages []string
			for len(stages) < 3 {
				m, err := protocol.ReadMessage(conn)
				if err != nil {
					t.Fatal(err)
				}
				if m.Kind == protocol.KindPackageInstall {
					ack = true
				}
				if m.Kind == protocol.KindError {
					t.Fatalf("install refused: %+v", m)
				}
				if m.Kind == protocol.KindInstallProgress {
					var p protocol.InstallProgress
					if json.Unmarshal(m.Data, &p) != nil {
						t.Fatal("invalid progress")
					}
					if p.Package != "wanctl" || p.Version != "v0.27.0-beta.1" {
						t.Fatal(p)
					}
					stages = append(stages, p.Stage)
				}
			}
			if !ack || strings.Join(stages, ",") != "downloading,installing,installing" {
				t.Fatalf("ack %v, stages %v", ack, stages)
			}
		})
	}
}
