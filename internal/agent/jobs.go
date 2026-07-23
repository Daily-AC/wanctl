package agent

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"wanctl/internal/server"
)

// jobRetain is how long a finished job's output stays pollable after it ends,
// so a controller that started a command can still fetch its tail and exit code
// minutes later.
const jobRetain = time.Hour

// job is a background command started via exec_async. Its output and exit status
// outlive the controller connection that launched it: a controller can start a
// long command, disconnect, and poll for the result later — escaping any
// per-request timeout (issue #2) and giving a once-orphaned command a queryable
// exit code (issue #16).
type job struct {
	id      string
	cmd     string
	started time.Time

	mu       sync.Mutex
	out      []byte // merged stdout+stderr accumulated so far
	done     bool
	code     int
	finished time.Time
}

// jobWriter funnels the running command's merged output into the job buffer
// under the lock so a concurrent poll always sees a consistent prefix.
type jobWriter struct{ j *job }

func (w jobWriter) Write(p []byte) (int, error) {
	w.j.mu.Lock()
	w.j.out = append(w.j.out, p...)
	w.j.mu.Unlock()
	return len(p), nil
}

// snapshot returns output from offset onward plus the current status. offset is
// clamped into range so a stale/over-long offset can't panic.
func (j *job) snapshot(offset int64) (newOut []byte, total int64, done bool, code int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	total = int64(len(j.out))
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	newOut = append([]byte(nil), j.out[offset:]...)
	return newOut, total, j.done, j.code
}

type jobStore struct {
	mu   sync.Mutex
	jobs map[string]*job
}

func newJobStore() *jobStore { return &jobStore{jobs: map[string]*job{}} }

func (s *jobStore) get(id string) *job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[id]
}

// start launches command in a fresh shell and returns the new job's id. The
// command runs in its own goroutine; the agent process (a long-lived daemon)
// keeps it alive after the launching connection closes.
func (s *jobStore) start(shell, command, cwd string) (string, error) {
	idb := make([]byte, 12)
	if _, err := rand.Read(idb); err != nil {
		return "", err
	}
	j := &job{id: hex.EncodeToString(idb), cmd: command, started: time.Now()}

	s.mu.Lock()
	s.gcLocked()
	s.jobs[j.id] = j
	s.mu.Unlock()

	go func() {
		code, err := server.RunOneShot(shell, command, cwd, jobWriter{j})
		j.mu.Lock()
		if err != nil {
			j.out = append(j.out, []byte("\n[wanctl: job error: "+err.Error()+"]\n")...)
			code = -1
		}
		j.done = true
		j.code = code
		j.finished = time.Now()
		j.mu.Unlock()
	}()
	return j.id, nil
}

// gcLocked drops jobs that finished more than jobRetain ago. Caller holds s.mu.
func (s *jobStore) gcLocked() {
	cutoff := time.Now().Add(-jobRetain)
	for id, j := range s.jobs {
		j.mu.Lock()
		expired := j.done && j.finished.Before(cutoff)
		j.mu.Unlock()
		if expired {
			delete(s.jobs, id)
		}
	}
}
