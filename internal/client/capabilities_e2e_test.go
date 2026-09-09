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

type grantACL struct{ id, perms string }

func (p grantACL) ACLPerms(callerNS, targetNS, device string) (string, bool) {
	return p.perms, callerNS == "shared" && targetNS == "owner" && device == p.id
}

// A shared device gives its grantee what the owner has (ADR 0007). The value
// stored in acl.perms is not read, so the same end-to-end assertions hold for a
// grant written as "read" and one written as "exec". What still constrains the
// grantee is the device: its mode and rules gate every request, and its
// console stays with whoever the device trusts to administer it.
func TestSharedSessionHasTheOwnersCapabilities(t *testing.T) {
	for _, transportName := range []string{"ws", "http"} {
		for _, storedGrant := range []string{"read", "exec"} {
			t.Run(transportName+"/"+storedGrant, func(t *testing.T) {
				c, ctx := startCapabilityFixture(t, transportName, storedGrant)

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

				written := filepath.Join(t.TempDir(), "write.txt")
				if err := c.PushBytes(ctx, "owner/home-pc", written, []byte("x"), 0o644); err != nil {
					t.Fatalf("push: %v", err)
				}

				var logs bytes.Buffer
				if err := c.LogsTo(ctx, "owner/home-pc", "", "", "", 1, &logs); err != nil {
					t.Fatalf("logs: %v", err)
				}

				// Console is not a capability the relay withholds any more. The
				// device withholds it, from every controller that is not one of
				// its console administrators - which is how approvals, rules and
				// mode stay with the owner.
				_, err = c.OpenConsole(ctx, "owner/home-pc")
				var rejected *RejectError
				if !errors.As(err, &rejected) || !strings.Contains(rejected.Reason, "console administrator") {
					t.Fatalf("console error = %v, want a device-side console-administrator refusal", err)
				}
			})
		}
	}
}

func startCapabilityFixture(t *testing.T, transportName, grant string) (*Client, context.Context) {
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
	r.SetACL(grantACL{id: ag.DeviceID(), perms: grant})
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
