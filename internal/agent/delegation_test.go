package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"wanctl/internal/client"
	"wanctl/internal/delegation"
	"wanctl/internal/eventlog"
	"wanctl/internal/policy"
	"wanctl/internal/relay"
	"wanctl/internal/transport"
)

type agentGrantTokens struct {
	mu      sync.Mutex
	access  delegation.Access
	revoked bool
	other   map[string]delegation.Access
}

func (s *agentGrantTokens) Resolve(token string) (string, bool) { return "alice", token == "owner" }
func (s *agentGrantTokens) ResolveAccess(token string) (delegation.Access, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if token == "owner" {
		return delegation.Access{Namespace: "alice", CredentialID: "owner"}, true
	}
	if access, ok := s.other[token]; ok {
		return access, !s.revoked && time.Now().Before(access.ExpiresAt)
	}
	return s.access, token == "delegate" && !s.revoked && time.Now().Before(s.access.ExpiresAt)
}

func (s *agentGrantTokens) issue(token string, access delegation.Access) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.other == nil {
		s.other = map[string]delegation.Access{}
	}
	s.other[token] = access
}
func (s *agentGrantTokens) revoke() { s.mu.Lock(); s.revoked = true; s.mu.Unlock() }

type delegationFixture struct {
	a            *Agent
	c            *client.Client
	tokens       *agentGrantTokens
	ctx          context.Context
	target       string
	relayURL     string
	ctlTransport string
}

func startDelegationFixture(t *testing.T, agentTransport, controllerTransport string, mode policy.Mode, autoTrust bool, ttl time.Duration) delegationFixture {
	t.Helper()
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	cid, err := transport.IdentityFromSeed(bytes.Repeat([]byte{19}, 32), "delegated-test-controller")
	if err != nil {
		t.Fatal(err)
	}
	ts := &agentGrantTokens{}
	r := relay.New(ts)
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	base := "ws" + strings.TrimPrefix(srv.URL, "http")
	a, err := New(Options{RelayURL: base, Token: "owner", Name: "allowed", Mode: mode, AutoYes: autoTrust, Transport: agentTransport})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	target := "alice/" + a.DeviceID()
	ts.access = delegation.Access{Namespace: "alice", CredentialID: "delegated-credential", Delegated: true, GrantID: "delegated-grant", ExpiresAt: time.Now().Add(ttl), ControllerFingerprint: cid.Fingerprint, Devices: []delegation.Device{{Namespace: "alice", ID: a.DeviceID(), Fingerprint: a.id.Fingerprint}}}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	go a.Run(ctx)
	known := transport.NewMemStore()
	if err := known.Pin(target, a.id.Fingerprint, false); err != nil {
		t.Fatal(err)
	}
	c := client.NewWith(cid, known, base, "delegate", controllerTransport)
	c.SetLabel("delegated test")
	for deadline := time.Now().Add(3 * time.Second); ; {
		peers, err := c.Peers(ctx)
		if err == nil && len(peers) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent never registered: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return delegationFixture{a: a, c: c, tokens: ts, ctx: ctx, target: target, relayURL: base, ctlTransport: controllerTransport}
}

func TestDelegatedDevicesStillEnforcePolicyAndTrust(t *testing.T) {
	for _, trust := range []bool{false, true} {
		t.Run(map[bool]string{false: "unpaired", true: "paired-but-denied"}[trust], func(t *testing.T) {
			f := startDelegationFixture(t, "ws", "http", policy.ModeNormal, trust, time.Minute)
			f.a.setApprover(policy.DenyApprover{})
			var out bytes.Buffer
			_, err := f.c.ExecTo(f.ctx, client.ExecRequest{Target: f.target, Command: "echo unauthorized", OneShot: true}, &out, io.Discard)
			var rejected *client.RejectError
			if !errors.As(err, &rejected) || out.Len() != 0 {
				t.Fatalf("device boundary bypassed: err=%v out=%q", err, out.String())
			}
			want := "approve"
			if trust {
				want = "policy"
			}
			if !strings.Contains(rejected.Reason, want) {
				t.Fatalf("wrong denial: %s", rejected.Reason)
			}
		})
	}
}

func TestDelegatedPairingReturnsLinkWhilePortalIsWatching(t *testing.T) {
	for _, carrier := range []string{"ws", "http"} {
		t.Run(carrier, func(t *testing.T) {
			t.Setenv("WANCTL_PORTAL", "https://portal.example")
			f := startDelegationFixture(t, carrier, "http", policy.ModeNormal, false, time.Minute)
			f.a.setApprover(policy.DenyApprover{})
			_, unsubscribe := f.a.console.Subscribe()
			defer unsubscribe()
			defer f.a.console.DecidePair(f.c.Identity().Fingerprint, false)
			ctx, cancel := context.WithTimeout(f.ctx, 2*time.Second)
			defer cancel()
			_, err := f.c.ExecTo(ctx, client.ExecRequest{Target: f.target, Command: "echo blocked", OneShot: true}, io.Discard, io.Discard)
			var rejected *client.RejectError
			if !errors.As(err, &rejected) || rejected.PairingURL == "" || !strings.Contains(rejected.Reason, "paired") {
				t.Fatalf("watching portal hid pairing response: %v", err)
			}
			if f.a.known.Has(f.c.Identity().Fingerprint) {
				t.Fatal("unapproved controller was trusted")
			}
			if !f.a.console.DecidePair(f.c.Identity().Fingerprint, true) {
				t.Fatal("pairing request was not retained for owner approval")
			}
			_, err = f.c.ExecTo(f.ctx, client.ExecRequest{Target: f.target, Command: "echo still blocked by policy", OneShot: true}, io.Discard, io.Discard)
			if !errors.As(err, &rejected) || !strings.Contains(rejected.Reason, "policy") || !f.a.known.Has(f.c.Identity().Fingerprint) {
				t.Fatalf("owner pairing was not applied without changing policy: %v", err)
			}
		})
	}
}

func TestDelegatedUseOverAllCarriersAndNoManagement(t *testing.T) {
	for _, agentTransport := range []string{"ws", "http"} {
		for _, controllerTransport := range []string{"ws", "http"} {
			t.Run(agentTransport+"-agent/"+controllerTransport+"-controller", func(t *testing.T) {
				f := startDelegationFixture(t, agentTransport, controllerTransport, policy.ModeBypass, true, time.Minute)
				var stdout bytes.Buffer
				code, err := f.c.ExecTo(f.ctx, client.ExecRequest{Target: f.target, Command: "printf 'delegated-ok'", OneShot: true}, &stdout, io.Discard)
				if err != nil || code != 0 || stdout.String() != "delegated-ok" {
					t.Fatalf("exec: %d %v %q", code, err, stdout.String())
				}
				path := filepath.Join(t.TempDir(), "created.txt")
				if err := f.c.PushBytes(f.ctx, f.target, path, []byte("from delegation"), 0600); err != nil {
					t.Fatalf("push: %v", err)
				}
				local := filepath.Join(t.TempDir(), "read.txt")
				if err := f.c.Pull(f.ctx, f.target, path, local); err != nil {
					t.Fatalf("pull: %v", err)
				}
				got, _ := os.ReadFile(local)
				if string(got) != "from delegation" {
					t.Fatalf("file mismatch: %q", got)
				}
				if err := f.c.LogsTo(f.ctx, f.target, "", "", "", 1, io.Discard); err != nil {
					t.Fatalf("logs: %v", err)
				}
				if _, err := f.c.OpenConsole(f.ctx, f.target); err == nil {
					t.Fatal("delegation opened console")
				}
				if _, err := f.c.ExecAsync(f.ctx, f.target, "echo forbidden", ""); err == nil {
					t.Fatal("delegation started detached job")
				}
				if _, err := f.c.ExecTo(f.ctx, client.ExecRequest{Target: f.target, Command: "echo forbidden"}, io.Discard, io.Discard); err == nil {
					t.Fatal("delegation used persistent shell")
				}
				otherID, _ := transport.IdentityFromSeed(bytes.Repeat([]byte{23}, 32), "other-controller")
				pins := transport.NewMemStore()
				pins.Pin(f.target, f.a.id.Fingerprint, false)
				other := client.NewWith(otherID, pins, f.relayURL, "delegate", controllerTransport)
				other.SetLabel("wrong controller")
				if _, err := other.ExecTo(f.ctx, client.ExecRequest{Target: f.target, Command: "echo forbidden", OneShot: true}, io.Discard, io.Discard); err == nil {
					t.Fatal("credential used by wrong fingerprint")
				}
				// A reused fingerprint and arbitrary label cannot merge grants in
				// the audit log: each operation retains its authenticated session.
				second := f.tokens.access
				second.GrantID, second.CredentialID = "second-grant", "second-credential"
				f.tokens.issue("delegate-two", second)
				reused := client.NewWith(f.c.Identity(), pins, f.relayURL, "delegate-two", controllerTransport)
				reused.SetLabel("claimed grant: fabricated-grant")
				if _, err := reused.ExecTo(f.ctx, client.ExecRequest{Target: f.target, Command: "printf second-grant-operation", OneShot: true}, io.Discard, io.Discard); err != nil {
					t.Fatalf("second grant: %v", err)
				}
				assertDelegationAudit(t, f.a, map[string]string{"delegated-grant": "delegated-credential", "second-grant": "second-credential"})
			})
		}
	}
}

func assertDelegationAudit(t *testing.T, a *Agent, grants map[string]string) {
	t.Helper()
	events, err := a.log.Read(eventlog.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	connections := map[string]eventlog.Event{}
	for _, e := range events {
		if e.Type != "connect" {
			continue
		}
		if e.SessionID == "" || grants[e.GrantID] != e.CredentialID || e.CredentialID == "" {
			t.Fatalf("connect attribution missing: %+v", e)
		}
		if e.Decision == "accepted" {
			connections[e.SessionID] = e
		}
	}
	counts := map[string]int{}
	seenGrants := map[string]bool{}
	for _, e := range events {
		switch e.Type {
		case "exec", "file", "logs":
		default:
			continue
		}
		connected, ok := connections[e.SessionID]
		if !ok || e.GrantID != connected.GrantID || e.CredentialID != connected.CredentialID || e.PeerFP != connected.PeerFP {
			t.Fatalf("operation cannot be joined to authenticated connection: %+v", e)
		}
		counts[e.Type]++
		seenGrants[e.GrantID] = true
	}
	if counts["exec"] < 2 || counts["file"] < 2 || counts["logs"] < 1 || len(seenGrants) != len(grants) {
		t.Fatalf("missing delegated operation audit: counts=%v grants=%v", counts, seenGrants)
	}
}

type delayedDelegationApproval struct {
	entered, allow chan struct{}
	once           sync.Once
}

func (a *delayedDelegationApproval) Ask(policy.Request) policy.Decision {
	a.once.Do(func() { close(a.entered) })
	<-a.allow
	return policy.Decision{Allow: true, Remember: true, Scope: policy.ScopeGlobal}
}

func TestDelegatedApprovalCannotRunAfterRevoke(t *testing.T) {
	for _, agentTransport := range []string{"ws", "http"} {
		t.Run(agentTransport, func(t *testing.T) {
			f := startDelegationFixture(t, agentTransport, "http", policy.ModeNormal, true, time.Minute)
			approval := &delayedDelegationApproval{entered: make(chan struct{}), allow: make(chan struct{})}
			f.a.setApprover(approval)
			path := filepath.Join(t.TempDir(), "must-not-exist")
			done := make(chan error, 1)
			go func() {
				_, err := f.c.ExecTo(f.ctx, client.ExecRequest{Target: f.target, Command: "echo bad > " + path, OneShot: true}, io.Discard, io.Discard)
				done <- err
			}()
			select {
			case <-approval.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("approval not reached")
			}
			f.tokens.revoke()
			close(approval.allow) // deliberately before the one-second relay sweeper
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("late approval succeeded")
				}
			case <-time.After(4 * time.Second):
				t.Fatal("late approval hung")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("revoked pending command created file: %v", err)
			}
			if f.a.engine.Allowed(policy.Request{Kind: policy.KindExec, Cmd: "another command", Peer: f.c.Identity().Fingerprint}) {
				t.Fatal("late approval persisted a policy rule")
			}
		})
	}
}

func TestDelegatedActiveCommandsStopOnRevokeAndExpiry(t *testing.T) {
	for _, agentTransport := range []string{"ws", "http"} {
		for _, controllerTransport := range []string{"ws", "http"} {
			for _, revoke := range []bool{false, true} {
				name := agentTransport + "-" + controllerTransport + "-" + map[bool]string{false: "expiry", true: "revoke"}[revoke]
				t.Run(name, func(t *testing.T) {
					ttl := 3 * time.Second
					if revoke {
						ttl = time.Minute
					}
					f := startDelegationFixture(t, agentTransport, controllerTransport, policy.ModeBypass, true, ttl)
					command, pidFile := remoteProbe(t)
					done := make(chan error, 1)
					go func() {
						_, err := f.c.ExecTo(f.ctx, client.ExecRequest{Target: f.target, Command: command, OneShot: true}, io.Discard, io.Discard)
						done <- err
					}()
					pid := waitForRemotePID(t, pidFile)
					t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
					if revoke {
						f.tokens.revoke()
					}
					select {
					case err := <-done:
						if err == nil {
							t.Fatal("revoked/expired command reported success")
						}
					case <-time.After(6 * time.Second):
						t.Fatal("command still connected")
					}
					if !remoteGone(pid, 3*time.Second) {
						t.Fatal("revoked/expired command process survived")
					}
				})
			}
		}
	}
}
