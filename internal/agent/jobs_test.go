package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	expectedMaxConcurrentJobs = 4
	expectedMaxJobOutput      = 8 << 20
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

func TestJobWriterCapsOutput(t *testing.T) {
	j := &job{}
	payload := []byte(strings.Repeat("x", expectedMaxJobOutput+1))
	if _, err := (jobWriter{j: j}).Write(payload); err != nil {
		t.Fatal(err)
	}
	if len(j.out) > expectedMaxJobOutput {
		t.Fatalf("job retained %d output bytes, limit %d", len(j.out), expectedMaxJobOutput)
	}
	if !strings.Contains(string(j.out[len(j.out)-len(jobOutputTruncated):]), "output truncated") {
		t.Fatal("truncated output does not carry an explicit marker")
	}
}

func TestJobStoreRejectsExcessConcurrentJobs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	s := newJobStore()
	for i := 0; i < expectedMaxConcurrentJobs; i++ {
		if _, err := s.start("/bin/sh", "sleep 0.5"); err != nil {
			t.Fatalf("start job %d: %v", i, err)
		}
	}
	if id, err := s.start("/bin/sh", "sleep 0.5"); err == nil {
		t.Fatalf("excess concurrent job was accepted as %s", id)
	}
}

func TestJobStoreTimesOutRunningJob(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	l := defaultJobLimits()
	l.runTimeout = 50 * time.Millisecond
	s := newJobStoreWithLimits(l)
	id, err := s.start("/bin/sh", "sleep 5")
	if err != nil {
		t.Fatal(err)
	}
	j := s.get(id)
	waitForJob(t, j)
	out, _, done, code := j.snapshot(0)
	if !done || code != -1 {
		t.Fatalf("timed-out job: done=%v code=%d", done, code)
	}
	if !strings.Contains(string(out), "timed out") {
		t.Fatalf("timed-out job output = %q", out)
	}
}

func TestJobStoreGCsOnCompletionToRetainedBudgets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	for _, tt := range []struct {
		name        string
		maxRetained int
		maxBytes    int
	}{
		{name: "job count", maxRetained: 1, maxBytes: 100},
		{name: "retained bytes", maxRetained: 10, maxBytes: 8},
	} {
		t.Run(tt.name, func(t *testing.T) {
			l := defaultJobLimits()
			l.maxRetained = tt.maxRetained
			l.maxBytes = tt.maxBytes
			s := newJobStoreWithLimits(l)
			var jobs []*job
			for _, command := range []string{"printf 123456", "printf abcdef"} {
				id, err := s.start("/bin/sh", command)
				if err != nil {
					t.Fatal(err)
				}
				jobs = append(jobs, s.get(id))
			}
			for _, j := range jobs {
				waitForJob(t, j)
			}
			deadline := time.Now().Add(time.Second)
			for {
				s.mu.Lock()
				count, running := len(s.jobs), s.running
				s.mu.Unlock()
				if count <= 1 && running == 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("completion GC left jobs=%d running=%d", count, running)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

func waitForJob(t *testing.T, j *job) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, _, done, _ := j.snapshot(0)
		if done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job did not finish")
}
