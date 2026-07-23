package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestJobStore_RunsCapturesOutputAndExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	s := newJobStore()
	id, err := s.start("/bin/sh", "printf 'hello\\n'; exit 7", "")
	if err != nil {
		t.Fatal(err)
	}
	j := s.get(id)
	if j == nil {
		t.Fatal("job not found right after start")
	}

	// Poll until done (or time out).
	deadline := time.Now().Add(5 * time.Second)
	var done bool
	var code int
	var total int64
	for time.Now().Before(deadline) {
		_, total, done, code = j.snapshot(0)
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !done {
		t.Fatal("job never finished")
	}
	if code != 7 {
		t.Fatalf("exit code = %d, want 7", code)
	}
	out, _, _, _ := j.snapshot(0)
	if string(out) != "hello\n" {
		t.Fatalf("output = %q, want %q", out, "hello\n")
	}

	// Incremental offset: from the end there should be nothing new.
	tail, _, _, _ := j.snapshot(total)
	if len(tail) != 0 {
		t.Fatalf("snapshot from total offset should be empty, got %q", tail)
	}
	// Over-long offset must clamp, not panic.
	if over, _, _, _ := j.snapshot(total + 9999); len(over) != 0 {
		t.Fatalf("over-long offset should clamp to empty, got %q", over)
	}
}

func TestJobStore_CwdWithSpecialCharacters(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	cwd := filepath.Join(t.TempDir(), `dir with spaces";semi`)
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}

	s := newJobStore()
	id, err := s.start("/bin/sh", "pwd", cwd)
	if err != nil {
		t.Fatal(err)
	}
	j := s.get(id)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, _, done, code := j.snapshot(0)
		if done {
			if code != 0 {
				t.Fatalf("exit code = %d, output = %q", code, out)
			}
			if got := strings.TrimSpace(string(out)); got != cwd {
				t.Fatalf("pwd = %q, want %q", got, cwd)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job never finished")
}

func TestJobStore_GCDropsExpiredFinishedJobs(t *testing.T) {
	s := newJobStore()
	old := &job{id: "old", done: true, finished: time.Now().Add(-2 * jobRetain)}
	fresh := &job{id: "fresh", done: true, finished: time.Now()}
	running := &job{id: "running", done: false}
	s.jobs["old"], s.jobs["fresh"], s.jobs["running"] = old, fresh, running

	s.mu.Lock()
	s.gcLocked()
	s.mu.Unlock()

	if s.get("old") != nil {
		t.Error("expired finished job should be GC'd")
	}
	if s.get("fresh") == nil {
		t.Error("recently finished job must be retained")
	}
	if s.get("running") == nil {
		t.Error("running job must never be GC'd")
	}
}
