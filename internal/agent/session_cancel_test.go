package agent

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"runtime"
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
	if runtime.GOOS == "windows" {
		t.Skip("observes the device-side process with POSIX signals")
	}
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

// The acceptance from #46: a session command dies with the controller, and the
// session it ran in is still usable afterwards with the cwd it had.
func TestSessionCommandCancelledWhenControllerDisconnects(t *testing.T) {
	base := sessionCancelRig(t)
	dr := reconnect(t, base)

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if out, code, reason := execSession(t, dr, "cd "+dir); code != 0 || reason != "" {
		t.Fatalf("cd = %d %q out=%q", code, reason, out)
	}

	pid := startSessionCommand(t, dr)
	dr.Conn.Close()

	if !remoteGone(pid, 3*time.Second) {
		t.Fatalf("device-side process %d survived the controller disconnecting", pid)
	}

	next := reconnect(t, base)
	defer next.Conn.Close()
	out, code, reason := execSession(t, next, "pwd")
	if code != 0 || reason != "" {
		t.Fatalf("command after the cancel = %d %q", code, reason)
	}
	if got := strings.TrimSpace(out); got != dir {
		t.Fatalf("session cwd after the cancel = %q, want %q (the shell was not kept)", got, dir)
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
	if _, code, reason := execSession(t, dr, "cd "+dir); code != 0 || reason != "" {
		t.Fatalf("cd = %d %q", code, reason)
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

	out, code, reason := execSession(t, dr, "pwd")
	if code != 0 || reason != "" {
		t.Fatalf("command after the cancel frame = %d %q", code, reason)
	}
	if got := strings.TrimSpace(out); got != dir {
		t.Fatalf("session cwd after the cancel frame = %q, want %q", got, dir)
	}
}
