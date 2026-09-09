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

type grantACL struct {
	id     string
	manage bool
}

func (p grantACL) ACLGrant(callerNS, targetNS, device string) (relay.Grant, bool) {
	return relay.Grant{Manage: p.manage}, callerNS == "shared" && targetNS == "owner" && device == p.id
}

// A share gives its grantee the owner's *use* of the device: exec, files and
// logs, over either transport, with the device's own mode and rules deciding
// each request. Management -- the device's approvals, rules and mode -- is one
// switch the owner sets per share, and it is the only thing that differs
// between the two runs below. See ADR 0007.
func TestSharedSessionHasTheOwnersCapabilities(t *testing.T) {
	for _, transportName := range []string{"ws", "http"} {
		for _, manage := range []bool{false, true} {
			name := transportName + "/use-only"
			if manage {
				name = transportName + "/manage"
			}
			t.Run(name, func(t *testing.T) {
				c, ctx := startCapabilityFixture(t, transportName, manage)

				var stdout bytes.Buffer
				code, err := c.ExecTo(ctx, ExecRequest{Target: "owner/home-pc", Command: "echo allowed", OneShot: true}, &stdout, &bytes.Buffer{})
				if err != nil || code != 0 || strings.TrimSpace(stdout.String()) != "allowed" {
					t.Fatalf("exec: code=%d err=%v stdout=%q", code, err, stdout.String())
				}

				remote := filepath.Join(t.TempDir(), "remote.txt")
				if err := os.WriteFile(remote, []byte("readable"), 0o644); err != nil {
					t.Fatal(err)
				}
				local := filepath.Join(t.TempDir(), "local.txt")
				if err := c.Pull(ctx, "owner/home-pc", remote, local); err != nil {
					t.Fatalf("pull: %v", err)
				}
				if got, _ := os.ReadFile(local); string(got) != "readable" {
					t.Fatalf("pulled content = %q", got)
				}

				if err := c.PushBytes(ctx, "owner/home-pc", filepath.Join(t.TempDir(), "write.txt"), []byte("x"), 0o644); err != nil {
					t.Fatalf("push: %v", err)
				}

				var logs bytes.Buffer
				if err := c.LogsTo(ctx, "owner/home-pc", "", "", "", 1, &logs); err != nil {
					t.Fatalf("logs: %v", err)
				}

				// Without the switch the relay withholds the console capability
				// outright. With it, the session carries the capability and the
				// device decides: this controller is not one of its console
				// administrators, so it still says no. Two different refusals,
				// and the difference is what the switch does.
				_, err = c.OpenConsole(ctx, "owner/home-pc")
				var rejected *RejectError
				if !errors.As(err, &rejected) {
					t.Fatalf("console error = %v, want a reject", err)
				}
				want := "session capability denied: console"
				if manage {
					want = "console administrator"
				}
				if !strings.Contains(rejected.Reason, want) {
					t.Fatalf("console refusal = %q, want one mentioning %q", rejected.Reason, want)
				}
			})
		}
	}
}

func startCapabilityFixture(t *testing.T, transportName string, manage bool) (*Client, context.Context) {
	t.Helper()
	r := relay.New(relay.EnvTokenStore("owner-token:owner,shared-token:shared"))
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	relayURL := srv.URL
	if transportName == "ws" {
		relayURL = "ws" + strings.TrimPrefix(srv.URL, "http")
	}

	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	agentID, err := transport.LoadOrCreateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	ag, err := agent.New(agent.Options{
		RelayURL: relayURL, Token: "owner-token", Name: "home-pc",
		AutoYes: true, Mode: policy.ModeBypass, Transport: transportName,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.SetACL(grantACL{id: ag.DeviceID(), manage: manage})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	go ag.Run(ctx)
	if transportName == "http" {
		time.Sleep(300 * time.Millisecond)
	} else {
		time.Sleep(200 * time.Millisecond)
	}

	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	id, err := transport.LoadOrCreateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	known, err := transport.OpenStore("known_servers.json")
	if err != nil {
		t.Fatal(err)
	}
	c := NewWith(id, known, relayURL, "shared-token", transportName)
	if _, err := c.PinServer(ctx, "owner/home-pc", agentID.Fingerprint, false); err != nil {
		t.Fatal(err)
	}
	return c, ctx
}
