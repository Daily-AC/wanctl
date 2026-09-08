package client

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wanctl/internal/agent"
	"wanctl/internal/policy"
	"wanctl/internal/relay"
	"wanctl/internal/transport"
)

func TestSameNameDevicesRouteIndependentlyAcrossTransports(t *testing.T) {
	r := relay.New(relay.EnvTokenStore("tok:alice"))
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	type running struct {
		a    *agent.Agent
		stop func()
	}
	start := func(dir, name, version, tr string) running {
		t.Helper()
		t.Setenv("WANCTL_CONFIG_DIR", dir)
		a, err := agent.New(agent.Options{RelayURL: srv.URL, Token: "tok", Name: name, Version: version, Transport: tr, AutoYes: true, Mode: policy.ModeBypass})
		if err != nil {
			t.Fatal(err)
		}
		runCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- a.Run(runCtx) }()
		stopped := false
		close := func() {
			if stopped {
				return
			}
			stopped = true
			stop()
			a.Close()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("agent did not stop")
			}
		}
		t.Cleanup(close)
		return running{a, close}
	}
	dirA, dirB := t.TempDir(), t.TempDir()
	a := start(dirA, "same-name", "agent-A", "ws")
	b := start(dirB, "same-name", "agent-B", "http")
	if a.a.DeviceID() == b.a.DeviceID() {
		t.Fatal("independent installations reused the ID")
	}
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	id, err := transport.LoadOrCreateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	known := transport.NewMemStore()
	clients := []*Client{NewWith(id, known, srv.URL, "tok", "ws"), NewWith(id, known, srv.URL, "tok", "http")}
	wait := func(name string, condition func() bool) {
		t.Helper()
		for !condition() {
			select {
			case <-ctx.Done():
				t.Fatalf("waiting for %s: %v", name, ctx.Err())
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	wait("two same-name devices", func() bool { ds, _, err := clients[0].PeersWithAliases(ctx); return err == nil && len(ds) == 2 })
	for _, c := range clients {
		if _, err := c.resolve(ctx, "same-name"); err == nil {
			t.Fatal("ambiguous name selected an arbitrary agent")
		}
		for _, dev := range []struct{ id, version string }{{a.a.DeviceID(), "agent-A"}, {b.a.DeviceID(), "agent-B"}} {
			if _, ok := known.GetByName("alice/" + dev.id); !ok {
				trustServer(t, c, dev.id)
			}
			st, err := c.Status(ctx, dev.id)
			if err != nil || st.Name != "same-name" || st.Version != dev.version {
				t.Fatalf("wrong agent: %+v %v", st, err)
			}
			var stdout bytes.Buffer
			code, err := c.ExecTo(ctx, ExecRequest{Target: dev.id, Command: "printf verified", OneShot: true, Cwd: ""}, &stdout, &bytes.Buffer{})
			if err != nil || code != 0 || stdout.String() != "verified" {
				t.Fatalf("exec %s via %s: %d %q %v", dev.id, c.transport, code, stdout.String(), err)
			}
		}
	}
	firstID := a.a.DeviceID()
	a.stop()
	a = start(dirA, "renamed", "agent-A-renamed", "ws")
	if a.a.DeviceID() != firstID {
		t.Fatal("rename changed persistent ID")
	}
	wait("renamed registration", func() bool {
		_, labels, err := clients[0].PeersWithAliases(ctx)
		return err == nil && labels[firstID] == "renamed"
	})
	if got, err := clients[0].resolve(ctx, "alice/renamed"); err != nil || got != "alice/"+firstID {
		t.Fatalf("qualified rename resolution: %s %v", got, err)
	}
	if st, err := clients[0].Status(ctx, "renamed"); err != nil || st.Version != "agent-A-renamed" {
		t.Fatalf("rename lost trust: %+v %v", st, err)
	}
	if got, err := clients[0].resolve(ctx, "same-name"); err != nil || got != "alice/"+b.a.DeviceID() {
		t.Fatalf("remaining name: %s %v", got, err)
	}
	a.stop()
	if err := os.Remove(filepath.Join(dirA, "cert.pem")); err != nil {
		t.Fatal(err)
	}
	a = start(dirA, "rotated", "agent-A-rotated", "ws")
	if a.a.DeviceID() != firstID {
		t.Fatal("certificate rotation changed ID")
	}
	wait("rotated registration", func() bool {
		_, labels, err := clients[0].PeersWithAliases(ctx)
		return err == nil && labels[firstID] == "rotated"
	})
	_, err = clients[0].Status(ctx, firstID)
	var mismatch *transport.MismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("new certificate must fail the existing pin: %v", err)
	}
	if !strings.Contains(mismatch.Name, firstID) {
		t.Fatalf("mismatch not scoped to ID: %+v", mismatch)
	}
}
