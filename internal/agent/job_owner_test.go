package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"wanctl/internal/eventlog"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
)

// pollJob sends exec_poll and collects what comes back: the output frames and
// the control message that ends the answer.
func pollJob(t *testing.T, conn interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
}, id string) ([]byte, protocol.Message) {
	t.Helper()
	if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindExecPoll, JobID: id}); err != nil {
		t.Fatal(err)
	}
	var out []byte
	for {
		kind, payload, err := protocol.ReadFrame(conn)
		if err != nil {
			t.Fatal(err)
		}
		if kind == protocol.FrameStdout {
			out = append(out, payload...)
			continue
		}
		m, err := protocol.DecodeMessage(payload)
		if err != nil {
			t.Fatal(err)
		}
		return out, m
	}
}

// A background job's output belongs to the controller that started it. Its ID
// is written to the device's activity log, which people sharing the device
// can read, so the ID alone must not be what hands the output over: another
// paired controller polling it is told the job does not exist.
func TestAsyncJobOutputOnlyForItsController(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	a, err := New(Options{RelayURL: "http://unused", Token: "synthetic", Mode: policy.ModeBypass, Shell: "/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	secret := filepath.Join(t.TempDir(), "job-input.txt")
	if err := os.WriteFile(secret, []byte("owner-only-output"), 0o600); err != nil {
		t.Fatal(err)
	}

	owner := pipeController(t, a, 1)
	started := roundTrip(t, owner, protocol.Message{Kind: protocol.KindExecAsync, Command: "cat " + quoteSh(secret)})
	if started.Kind != protocol.KindOK || started.JobID == "" {
		t.Fatalf("job start: %+v", started)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, m := pollJob(t, owner, started.JobID); m.Kind == protocol.KindExit && !m.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	events, err := a.log.Read(eventlog.Filter{Type: "exec"})
	if err != nil {
		t.Fatal(err)
	}
	logged := false
	for _, e := range events {
		logged = logged || strings.Contains(e.Detail, started.JobID)
	}
	if !logged {
		t.Fatal("the job ID is expected in the activity log; this test assumes another reader can learn it")
	}

	other := pipeController(t, a, 2)
	out, m := pollJob(t, other, started.JobID)
	if bytes.Contains(out, []byte("owner-only-output")) {
		t.Fatal("another controller read the job's output")
	}
	if m.Kind != protocol.KindError || !strings.Contains(m.Reason, "unknown or expired job") {
		t.Fatalf("another controller's poll answered %+v; want the same answer as for a job that does not exist", m)
	}
	denied, err := a.log.Read(eventlog.Filter{Grep: "[poll " + started.JobID + "]"})
	if err != nil || len(denied) != 1 || !strings.HasPrefix(denied[0].Decision, "denied") {
		t.Fatalf("the refused poll left no trace for the owner: %+v %v", denied, err)
	}

	// The controller that started it still gets everything.
	out, m = pollJob(t, owner, started.JobID)
	if !bytes.Contains(out, []byte("owner-only-output")) || m.Kind != protocol.KindExit || m.Code != 0 {
		t.Fatalf("owner's poll: %q %+v", out, m)
	}
}
