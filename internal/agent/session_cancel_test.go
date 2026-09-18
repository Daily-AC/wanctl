//go:build !windows

package agent

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"wanctl/internal/policy"
	"wanctl/internal/protocol"
	"wanctl/internal/relay"
	"wanctl/internal/transport"
	"wanctl/internal/wsconn"
)

// sessionCancelRig is cancelRig with one controller identity held across
// reconnects: the persistent session is keyed by the controller's fingerprint,
// so a second connection only reaches the same session if it is the same
// controller.
func sessionCancelRig(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	t.Cleanup(srv.Close)
	base := "ws" + strings.TrimPrefix(srv.URL, "http")
	startAgent(t, base, policy.DenyApprover{}, policy.ModeBypass)
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir()) // the controller's, set after the agent read its own
	return base
}

// reconnect dials as the controller whose identity already lives in
// WANCTL_CONFIG_DIR, so it lands on the same device-side session.
func reconnect(t *testing.T, base string) *transport.DialResult {
	t.Helper()
	cid, err := transport.LoadOrCreateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	known, err := transport.OpenStore("known_servers.json")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	nc, _, err := wsconn.Dial(ctx, base+"/dial?token=tok&target=alice/home-pc", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	dr, err := transport.ClientHandshake(ctx, nc, "home-pc", cid, known)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	protocol.WriteMessage(dr.Conn, protocol.Message{Kind: protocol.KindHello, Role: "client", Name: "tester"})
	reply, _ := protocol.ReadMessage(dr.Conn)
	if reply.Kind != protocol.KindOK {
		t.Fatalf("hello: %s %s", reply.Kind, reply.Reason)
	}
	return dr
}

// execSession runs a command on the persistent-session path (no OneShot) and
// reads it to completion.
func execSession(t *testing.T, dr *transport.DialResult, command string) (string, int, string) {
	t.Helper()
	protocol.WriteMessage(dr.Conn, protocol.Message{Kind: protocol.KindExec, Command: command})
	var out strings.Builder
	for {
		ft, p, err := protocol.ReadFrame(dr.Conn)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if ft != protocol.FrameJSON {
			out.Write(p)
			continue
		}
		m, _ := protocol.DecodeMessage(p)
		switch m.Kind {
		case protocol.KindExit:
			return out.String(), m.Code, ""
		case protocol.KindReject, protocol.KindError:
			return out.String(), -1, m.Reason
		}
	}
}

// startSessionCommand sends a session command that parks a child and records its
// pid, and returns that pid once the device has started it.
func startSessionCommand(t *testing.T, dr *transport.DialResult) int {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	protocol.WriteMessage(dr.Conn, protocol.Message{
		Kind: protocol.KindExec, Command: "sleep 600 & echo $! > " + pidFile + "; wait",
	})
	pid := waitForRemotePID(t, pidFile)
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

// The acceptance from #46: a session command dies with the controller within a
// couple of seconds, and a following exec on the same target still works. It
// works on a *new* session, which is option (b): the cancelled one is gone, so
// the cwd and environment earlier commands set are gone with it.
func TestSessionCommandCancelledWhenControllerDisconnects(t *testing.T) {
	base := sessionCancelRig(t)
	dr := reconnect(t, base)

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if out, code, reason := execSession(t, dr, "cd "+dir+"; export WANCTL_MARK=set"); code != 0 || reason != "" {
		t.Fatalf("session setup = %d %q out=%q", code, reason, out)
	}
	if out, _, _ := execSession(t, dr, "pwd"); strings.TrimSpace(out) != dir {
		t.Fatalf("session cwd before the cancel = %q, want %q", strings.TrimSpace(out), dir)
	}

	pid := startSessionCommand(t, dr)
	start := time.Now()
	dr.Conn.Close()

	if !remoteGone(pid, 3*time.Second) {
		t.Fatalf("device-side process %d survived the controller disconnecting", pid)
	}
	t.Logf("device-side process ended %s after the controller stream closed", time.Since(start).Round(time.Millisecond))

	next := reconnect(t, base)
	defer next.Conn.Close()
	out, code, reason := execSession(t, next, "pwd; echo mark=[$WANCTL_MARK]")
	if code != 0 || reason != "" {
		t.Fatalf("command after the cancel = %d %q", code, reason)
	}
	if strings.Contains(out, dir) {
		t.Fatalf("the next command still sees the cancelled session's cwd %q; the session was not reset", dir)
	}
	if !strings.Contains(out, "mark=[]") {
		t.Fatalf("the next command still sees the cancelled session's environment: %q", out)
	}
}

// The same cancel arriving as a frame, on a connection that stays up: the
// command ends, the controller is told it was cancelled rather than that the
// command failed, and the next command runs on the same session.
func TestSessionCommandCancelledByCancelFrame(t *testing.T) {
	base := sessionCancelRig(t)
	dr := reconnect(t, base)
	defer dr.Conn.Close()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, code, reason := execSession(t, dr, "cd "+dir+"; export WANCTL_MARK=set"); code != 0 || reason != "" {
		t.Fatalf("session setup = %d %q", code, reason)
	}

	pid := startSessionCommand(t, dr)
	protocol.WriteMessage(dr.Conn, protocol.Message{Kind: protocol.KindCancel})

	dr.Conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		ft, payload, readErr := protocol.ReadFrame(dr.Conn)
		if readErr != nil {
			t.Fatalf("read after cancel: %v", readErr)
		}
		if ft != protocol.FrameJSON {
			continue
		}
		m, _ := protocol.DecodeMessage(payload)
		if m.Kind == protocol.KindExit {
			t.Fatalf("cancelled session command reported a normal exit (code %d)", m.Code)
		}
		if m.Kind == protocol.KindError {
			if !strings.Contains(m.Reason, "cancel") {
				t.Fatalf("cancel error reason = %q", m.Reason)
			}
			break
		}
	}
	dr.Conn.SetReadDeadline(time.Time{})

	if !remoteGone(pid, 3*time.Second) {
		t.Fatalf("device-side process %d survived the cancel frame", pid)
	}

	out, code, reason := execSession(t, dr, "pwd; echo mark=[$WANCTL_MARK]")
	if code != 0 || reason != "" {
		t.Fatalf("command after the cancel frame = %d %q", code, reason)
	}
	if strings.Contains(out, dir) || !strings.Contains(out, "mark=[]") {
		t.Fatalf("the command after the cancel frame ran on the old session: %q", out)
	}
}
