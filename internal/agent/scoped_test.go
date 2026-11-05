package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wanctl/internal/client"
	"wanctl/internal/delegation"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
	"wanctl/internal/transport"
)

// A scoped command token (docs/adr/0021-scoped-command-token.md) through a
// real relay, a real agent and a real client, on every carrier pairing. The
// device is in bypass mode with the controller paired, so the ONLY thing
// standing between the holder and an arbitrary command is the token's scope.
type scopedFixture struct {
	delegationFixture
	scoped  *client.Client
	command string
}

func startScopedFixture(t *testing.T, agentTransport, controllerTransport, command string, ttl time.Duration) scopedFixture {
	t.Helper()
	f := startDelegationFixture(t, agentTransport, controllerTransport, policy.ModeBypass, true, time.Minute)
	f.tokens.issue("wcs_scoped", delegation.Access{
		Namespace: "alice", CredentialID: "scoped-credential", Delegated: true, GrantID: "sc_1",
		ExpiresAt: time.Now().Add(ttl), Devices: f.tokens.access.Devices, ExecOnly: command,
	})
	pins := transport.NewMemStore()
	if err := pins.Pin(f.target, f.a.id.Fingerprint, false); err != nil {
		t.Fatal(err)
	}
	// Same controller key the device already paired: the token is not what
	// authorises the controller, the device's own pairing is.
	c := client.NewWith(f.c.Identity(), pins, f.relayURL, "wcs_scoped", controllerTransport)
	c.SetLabel("scoped test")
	return scopedFixture{delegationFixture: f, scoped: c, command: command}
}

func (f scopedFixture) run(command string) (string, error) {
	var out bytes.Buffer
	_, err := f.scoped.ExecTo(f.ctx, client.ExecRequest{Target: f.target, Command: command, OneShot: true}, &out, io.Discard)
	return out.String(), err
}

func TestScopedTokenRunsExactlyItsCommand(t *testing.T) {
	for _, agentTransport := range []string{"ws", "http"} {
		for _, controllerTransport := range []string{"ws", "http"} {
			t.Run(agentTransport+"-agent/"+controllerTransport+"-controller", func(t *testing.T) {
				marker := filepath.Join(t.TempDir(), "must-not-exist")
				command := "printf scoped-ok"
				f := startScopedFixture(t, agentTransport, controllerTransport, command, time.Minute)

				// The one thing it may do.
				if out, err := f.run(command); err != nil || out != "scoped-ok" {
					t.Fatalf("scoped command: %q %v", out, err)
				}
				// Every variation of it, each of which would create the marker or
				// print the scoped output if the device ran it.
				for _, other := range []string{
					"touch " + marker,
					command + " extra",
					command + "x",
					command + "; touch " + marker,
					command + " && touch " + marker,
					command + " | touch " + marker,
					command + "\ntouch " + marker,
					command + " $(touch " + marker + ")",
					command + " `touch " + marker + "`",
					command + " > " + marker,
					" " + command,
					command + " ",
					strings.ToUpper(command),
					"",
				} {
					out, err := f.run(other)
					var rejected *client.RejectError
					if err == nil || !errors.As(err, &rejected) || out != "" {
						t.Fatalf("command %q was not refused: out=%q err=%v", other, out, err)
					}
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatalf("a refused command ran: %v", err)
				}
			})
		}
	}
}

func TestScopedTokenRefusesEveryOtherVerbAndParameter(t *testing.T) {
	for _, carrier := range []string{"ws", "http"} {
		t.Run(carrier, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "must-not-exist")
			command := "printf scoped-ok"
			f := startScopedFixture(t, carrier, carrier, command, time.Minute)

			var out bytes.Buffer
			// The same command with the parameters a holder could add.
			for name, req := range map[string]client.ExecRequest{
				"cwd":      {Target: f.target, Command: command, OneShot: true, Cwd: dir},
				"elevate":  {Target: f.target, Command: command, OneShot: true, Elevate: true},
				"via":      {Target: f.target, Command: command, OneShot: true, Elevate: true, Via: "su"},
				"shell":    {Target: f.target, Command: command},
				"negspill": {Target: f.target, Command: command, OneShot: true, SpillAfter: -1},
			} {
				out.Reset()
				if _, err := f.scoped.ExecTo(f.ctx, req, &out, io.Discard); err == nil || out.Len() != 0 {
					t.Fatalf("%s: the scoped command ran with extra parameters: out=%q err=%v", name, out.String(), err)
				}
			}
			if _, err := f.scoped.ExecAsync(f.ctx, f.target, command, ""); err == nil {
				t.Fatal("scoped token started a detached job")
			}
			if err := f.scoped.PushBytes(f.ctx, f.target, marker, []byte("x"), 0600); err == nil {
				t.Fatal("scoped token wrote a file")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("push created a file: %v", err)
			}
			seed := filepath.Join(dir, "seed.txt")
			if err := os.WriteFile(seed, []byte("secret"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := f.scoped.Pull(f.ctx, f.target, seed, filepath.Join(t.TempDir(), "got.txt")); err == nil {
				t.Fatal("scoped token read a file")
			}
			if err := f.scoped.LogsTo(f.ctx, f.target, "", "", "", 1, io.Discard); err == nil {
				t.Fatal("scoped token read the device log")
			}
			if _, err := f.scoped.Status(f.ctx, f.target); err == nil {
				t.Fatal("scoped token read device status")
			}
			if _, err := f.scoped.OpenConsole(f.ctx, f.target); err == nil {
				t.Fatal("scoped token opened the console")
			}
			// And after all that the one allowed command still works: refusals
			// do not poison the token.
			if got, err := f.run(command); err != nil || got != "scoped-ok" {
				t.Fatalf("scoped command after refusals: %q %v", got, err)
			}
		})
	}
}

// The device decides from the message alone, so it must refuse a request that
// a modified client writes straight onto the wire, including fields this
// client never sets.
func TestScopedSessionRefusesAnythingButTheCanonicalRequest(t *testing.T) {
	cmd := "printf scoped-ok"
	canonical := protocol.Message{Kind: protocol.KindExec, Command: cmd, OneShot: true}
	if !scopedRequestAllowed(canonical, cmd) {
		t.Fatal("canonical request refused")
	}
	spill := canonical
	spill.SpillAfter = 4096
	if !scopedRequestAllowed(spill, cmd) {
		t.Fatal("spill request refused")
	}
	if !scopedRequestAllowed(protocol.Message{Kind: protocol.KindCancel}, cmd) {
		t.Fatal("bare cancel refused")
	}
	for name, mutate := range map[string]func(*protocol.Message){
		"command":     func(m *protocol.Message) { m.Command += " " },
		"kind":        func(m *protocol.Message) { m.Kind = protocol.KindExecAsync },
		"oneshot":     func(m *protocol.Message) { m.OneShot = false },
		"cwd":         func(m *protocol.Message) { m.Cwd = "/" },
		"elevate":     func(m *protocol.Message) { m.Elevate = true },
		"via":         func(m *protocol.Message) { m.Via = "su" },
		"script":      func(m *protocol.Message) { m.Script = "id" },
		"interp":      func(m *protocol.Message) { m.Interp = "sh" },
		"action":      func(m *protocol.Message) { m.Action = "x" },
		"desktop":     func(m *protocol.Message) { m.Desktop = &protocol.DesktopRequest{} },
		"workspace":   func(m *protocol.Message) { m.WorkspaceID = "w" },
		"request id":  func(m *protocol.Message) { m.RequestID = "r" },
		"path":        func(m *protocol.Message) { m.Path = "/etc/passwd" },
		"content":     func(m *protocol.Message) { m.Content = "x" },
		"edits":       func(m *protocol.Message) { m.Edits = []protocol.FileEdit{} },
		"job":         func(m *protocol.Message) { m.JobID = "j" },
		"negspill":    func(m *protocol.Message) { m.SpillAfter = -1 },
		"verdict":     func(m *protocol.Message) { m.Verdict = "y" },
		"grep":        func(m *protocol.Message) { m.Grep = "x" },
		"role":        func(m *protocol.Message) { m.Role = "client" },
		"cancel+data": func(m *protocol.Message) { m.Kind = protocol.KindCancel },
	} {
		m := canonical
		mutate(&m)
		if scopedRequestAllowed(m, cmd) {
			t.Fatalf("%s: modified request accepted: %+v", name, m)
		}
	}
	for _, kind := range []string{protocol.KindStatus, protocol.KindLogs, protocol.KindFilePut, protocol.KindFileGet, protocol.KindFileRead, protocol.KindFileEdit, protocol.KindFileWrite, protocol.KindDesktop, protocol.KindWorkspace, protocol.KindExecPoll, protocol.KindHello, "something-new"} {
		if scopedRequestAllowed(protocol.Message{Kind: kind}, cmd) {
			t.Fatalf("kind %q accepted", kind)
		}
	}
}

func TestScopedTokenRevocationAndExpiryStopTheNextRequest(t *testing.T) {
	for _, carrier := range []string{"ws", "http"} {
		t.Run("revoke/"+carrier, func(t *testing.T) {
			f := startScopedFixture(t, carrier, carrier, "printf scoped-ok", time.Minute)
			if out, err := f.run(f.command); err != nil || out != "scoped-ok" {
				t.Fatalf("before revoke: %q %v", out, err)
			}
			f.tokens.revoke()
			if out, err := f.run(f.command); err == nil || out != "" {
				t.Fatalf("revoked token still ran: %q %v", out, err)
			}
		})
		t.Run("expire/"+carrier, func(t *testing.T) {
			f := startScopedFixture(t, carrier, carrier, "printf scoped-ok", 1500*time.Millisecond)
			if out, err := f.run(f.command); err != nil || out != "scoped-ok" {
				t.Fatalf("before expiry: %q %v", out, err)
			}
			time.Sleep(1700 * time.Millisecond)
			if out, err := f.run(f.command); err == nil || out != "" {
				t.Fatalf("expired token still ran: %q %v", out, err)
			}
		})
	}
}

// The token narrows the device's policy, never widens it: with the device
// refusing the command, the scoped holder is refused too.
func TestScopedTokenStillMeetsDevicePolicy(t *testing.T) {
	f := startScopedFixture(t, "ws", "ws", "printf scoped-ok", time.Minute)
	f.a.setApprover(policy.DenyApprover{})
	f.a.engine.SetMode(policy.ModeNormal)
	var out bytes.Buffer
	_, err := f.scoped.ExecTo(f.ctx, client.ExecRequest{Target: f.target, Command: f.command, OneShot: true}, &out, io.Discard)
	var rejected *client.RejectError
	if !errors.As(err, &rejected) || out.Len() != 0 {
		t.Fatalf("scoped token bypassed device policy: out=%q err=%v", out.String(), err)
	}
}

func TestScopedTokenBoundToWrongDeviceOrAgentIsRefused(t *testing.T) {
	f := startScopedFixture(t, "ws", "ws", "printf scoped-ok", time.Minute)
	other := delegation.Access{
		Namespace: "alice", CredentialID: "wrong-device", Delegated: true, GrantID: "sc_2", ExpiresAt: time.Now().Add(time.Minute),
		Devices: []delegation.Device{{Namespace: "alice", ID: "some-other-device", Fingerprint: "SHA256:other"}}, ExecOnly: f.command,
	}
	f.tokens.issue("wcs_other", other)
	pins := transport.NewMemStore()
	pins.Pin(f.target, f.a.id.Fingerprint, false)
	c := client.NewWith(f.c.Identity(), pins, f.relayURL, "wcs_other", "ws")
	c.SetLabel("scoped test")
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	if _, err := c.ExecTo(ctx, client.ExecRequest{Target: f.target, Command: f.command, OneShot: true}, io.Discard, io.Discard); err == nil {
		t.Fatal("scoped token for another device reached this one")
	}
}

// Codex review finding: the wire decoder drops unknown keys and accepts
// duplicate or differently-cased keys, so the device compares the DECODED
// message. That is sound because the decoded message is also exactly what the
// device would execute; this test pins that: whatever spelling the bytes use,
// a request is accepted only if the struct that would run is the canonical one.
func TestScopedRequestJSONSpellingCannotSmuggleAnything(t *testing.T) {
	cmd := "printf scoped-ok"
	decode := func(raw string) protocol.Message {
		t.Helper()
		var m protocol.Message
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	for name, raw := range map[string]string{
		"unknown key":      `{"kind":"exec","command":"printf scoped-ok","oneshot":true,"future_field":"x"}`,
		"case alias":       `{"KIND":"exec","Command":"printf scoped-ok","OneShot":true}`,
		"overridden cwd":   `{"kind":"exec","command":"printf scoped-ok","oneshot":true,"cwd":"/","cwd":""}`,
		"overridden extra": `{"kind":"exec","command":"id","oneshot":true,"command":"printf scoped-ok"}`,
	} {
		m := decode(raw)
		if !scopedRequestAllowed(m, cmd) || m.Command != cmd || m.Cwd != "" || m.Elevate || m.Script != "" {
			t.Fatalf("%s: decoded %+v", name, m)
		}
	}
	for name, raw := range map[string]string{
		"duplicate command, last wins": `{"kind":"exec","command":"printf scoped-ok","oneshot":true,"command":"id"}`,
		"duplicate cwd, last wins":     `{"kind":"exec","command":"printf scoped-ok","oneshot":true,"cwd":"","cwd":"/"}`,
		"case alias of a real field":   `{"kind":"exec","command":"printf scoped-ok","oneshot":true,"CWD":"/"}`,
		"escaped key":                  `{"kind":"exec","command":"printf scoped-ok","oneshot":true,"cwd":"/"}`,
	} {
		if m := decode(raw); scopedRequestAllowed(m, cmd) {
			t.Fatalf("%s: accepted %+v", name, m)
		}
	}
}
