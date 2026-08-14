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

type grantACL string

func (p grantACL) ACLPerms(callerNS, targetNS, device string) (string, bool) {
	return string(p), callerNS == "shared" && targetNS == "owner" && device == "home-pc"
}

func TestSharedSessionCapabilities(t *testing.T) {
	for _, transportName := range []string{"ws", "http"} {
		t.Run(transportName+"/read", func(t *testing.T) {
			c, ctx := startCapabilityFixture(t, transportName, "read")
			remote := filepath.Join(t.TempDir(), "remote.txt")
			if err := os.WriteFile(remote, []byte("readable"), 0o644); err != nil {
				t.Fatal(err)
			}
			local := filepath.Join(t.TempDir(), "local.txt")
			if err := c.Pull(ctx, "owner/home-pc", remote, local); err != nil {
				t.Fatalf("read grant pull: %v", err)
			}
			if got, _ := os.ReadFile(local); string(got) != "readable" {
				t.Fatalf("pulled content = %q", got)
			}
			requireCapabilityReject(t, execError(ctx, c), "exec")
			requireCapabilityReject(t, c.PushBytes(ctx, "owner/home-pc", filepath.Join(t.TempDir(), "write.txt"), []byte("x"), 0o644), "write")
			requireCapabilityReject(t, c.LogsTo(ctx, "owner/home-pc", "", "", "", 1, &bytes.Buffer{}), "logs")
			_, err := c.OpenConsole(ctx, "owner/home-pc")
			requireCapabilityReject(t, err, "console")
		})

		t.Run(transportName+"/exec", func(t *testing.T) {
			c, ctx := startCapabilityFixture(t, transportName, "exec")
			var stdout bytes.Buffer
			code, err := c.ExecTo(ctx, ExecRequest{Target: "owner/home-pc", Command: "echo allowed", OneShot: true, Cwd: ""}, &stdout, &bytes.Buffer{})
			if err != nil || code != 0 || strings.TrimSpace(stdout.String()) != "allowed" {
				t.Fatalf("exec grant result: code=%d err=%v stdout=%q", code, err, stdout.String())
			}
			requireCapabilityReject(t, c.Pull(ctx, "owner/home-pc", filepath.Join(t.TempDir(), "missing"), filepath.Join(t.TempDir(), "local")), "read")
			requireCapabilityReject(t, c.PushBytes(ctx, "owner/home-pc", filepath.Join(t.TempDir(), "write.txt"), []byte("x"), 0o644), "write")
			requireCapabilityReject(t, c.LogsTo(ctx, "owner/home-pc", "", "", "", 1, &bytes.Buffer{}), "logs")
			_, err = c.OpenConsole(ctx, "owner/home-pc")
			requireCapabilityReject(t, err, "console")
		})
	}
}

func startCapabilityFixture(t *testing.T, transportName, grant string) (*Client, context.Context) {
	t.Helper()
	r := relay.New(relay.EnvTokenStore("owner-token:owner,shared-token:shared"))
	r.SetACL(grantACL(grant))
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

func execError(ctx context.Context, c *Client) error {
	_, err := c.ExecTo(ctx, ExecRequest{Target: "owner/home-pc", Command: "echo denied", OneShot: true, Cwd: ""}, &bytes.Buffer{}, &bytes.Buffer{})
	return err
}

func requireCapabilityReject(t *testing.T, err error, capability string) {
	t.Helper()
	var rejected *RejectError
	if !errors.As(err, &rejected) || !strings.Contains(rejected.Reason, "capability denied: "+capability) {
		t.Fatalf("error = %v, want %s capability rejection", err, capability)
	}
}
