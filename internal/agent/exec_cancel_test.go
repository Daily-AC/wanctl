package agent

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"wanctl/internal/policy"
	"wanctl/internal/protocol"
	"wanctl/internal/relay"
	"wanctl/internal/transport"
)

// remoteProbe is a one-shot command that parks a grandchild process on the
// device and records its pid. `sleep &` + `wait` is the shape a real long
// command has — the process to be killed is not the shell the agent started but
// something under it — so it is what the cancel path actually has to reach.
func remoteProbe(t *testing.T) (command, pidFile string) {
	t.Helper()
	pidFile = filepath.Join(t.TempDir(), "child.pid")
	return "sleep 60 & echo $! > " + pidFile + "; wait", pidFile
}

func waitForRemotePID(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(pidFile)
		if err == nil && len(strings.TrimSpace(string(b))) > 0 {
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				t.Fatalf("pid file %q: %v", b, err)
			}
			// The pid is written before `wait`, so give the process a moment to
			// actually be in the group the cancel hook kills.
			time.Sleep(200 * time.Millisecond)
			return pid
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("remote command never wrote its pid to %s", pidFile)
	return 0
}

// remoteGone reports whether pid stopped existing within d. The probe process is
// a grandchild of this test process, so it is reparented and reaped elsewhere
// and never lingers as a zombie.
func remoteGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return syscall.Kill(pid, 0) != nil
}

func cancelRig(t *testing.T) *transport.DialResult {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("observes the remote process with POSIX signals and /bin/sh job control")
	}
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	t.Cleanup(srv.Close)
	base := "ws" + strings.TrimPrefix(srv.URL, "http")
	startAgent(t, base, policy.DenyApprover{}, policy.ModeBypass)
	return connectController(t, base)
}

// A controller that goes away mid-command (Ctrl-C: the process dies and its
// stream ends) must take the command on the device with it. Before the fix the
// device ran -oneshot commands under context.Background(), so the shell and its
// children ran to completion with nobody left to read them (#37).
func TestOneShotCancelledWhenControllerDisconnects(t *testing.T) {
	dr := cancelRig(t)
	command, pidFile := remoteProbe(t)
	protocol.WriteMessage(dr.Conn, protocol.Message{Kind: protocol.KindExec, Command: command, OneShot: true})
	pid := waitForRemotePID(t, pidFile)
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })

	dr.Conn.Close()

	if !remoteGone(pid, 10*time.Second) {
		t.Fatalf("remote process %d survived the controller disconnecting", pid)
	}
}

// An explicit cancel frame ends the command without ending the session: the
// same connection keeps working afterwards.
func TestOneShotCancelledByCancelFrame(t *testing.T) {
	dr := cancelRig(t)
	defer dr.Conn.Close()
	command, pidFile := remoteProbe(t)
	protocol.WriteMessage(dr.Conn, protocol.Message{Kind: protocol.KindExec, Command: command, OneShot: true})
	pid := waitForRemotePID(t, pidFile)
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })

	protocol.WriteMessage(dr.Conn, protocol.Message{Kind: protocol.KindCancel})

	// The device reports the aborted command rather than a normal exit.
	deadline := time.Now().Add(10 * time.Second)
	dr.Conn.SetReadDeadline(deadline)
	for {
		ft, payload, err := protocol.ReadFrame(dr.Conn)
		if err != nil {
			t.Fatalf("read after cancel: %v", err)
		}
		if ft != protocol.FrameJSON {
			continue
		}
		m, _ := protocol.DecodeMessage(payload)
		if m.Kind == protocol.KindExit {
			t.Fatalf("cancelled command reported a normal exit (code %d)", m.Code)
		}
		if m.Kind == protocol.KindError {
			if !strings.Contains(m.Reason, "cancel") {
				t.Fatalf("cancel error reason = %q", m.Reason)
			}
			break
		}
	}
	dr.Conn.SetReadDeadline(time.Time{})

	if !remoteGone(pid, 10*time.Second) {
		t.Fatalf("remote process %d survived the cancel frame", pid)
	}

	// The read the device started to notice the cancel must be handed back to
	// its request loop, or this next command would deadlock or be lost.
	out, code, reason := execOnce(t, dr, "echo after-cancel", "")
	if code != 0 || reason != "" || !strings.Contains(out, "after-cancel") {
		t.Fatalf("command after cancel: out=%q code=%d reason=%q", out, code, reason)
	}
}
