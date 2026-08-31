package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"wanctl/internal/limits"
	"wanctl/internal/server"
)

// jobRetain is how long a finished job's output stays pollable after it ends,
// so a controller that started a command can still fetch its tail and exit code
// minutes later.
const jobRetain = limits.JobRetention

var errTooManyJobs = errors.New("too many concurrent background jobs")

const jobOutputTruncated = "\n[wanctl: job output truncated]\n"

type jobLimits struct {
	maxConcurrent int
	runTimeout    time.Duration
	maxOutput     int
	maxRetained   int
	maxBytes      int
	retain        time.Duration
}

func defaultJobLimits() jobLimits {
	return jobLimits{
		maxConcurrent: limits.MaxConcurrentJobs,
		runTimeout:    limits.JobRunTimeout,
		maxOutput:     limits.MaxJobOutputBytes,
		maxRetained:   limits.MaxRetainedJobs,
		maxBytes:      limits.MaxRetainedBytes,
		retain:        limits.JobRetention,
	}
}

// job is a background command started via exec_async. Its output and exit status
// outlive the controller connection that launched it: a controller can start a
// long command, disconnect, and poll for the result later — escaping any
// per-request timeout (issue #2) and giving a once-orphaned command a queryable
// exit code (issue #16).
type job struct {
	id      string
	cmd     string
	started time.Time

	mu        sync.Mutex
	out       []byte // merged stdout+stderr accumulated so far
	done      bool
	code      int
	finished  time.Time
	maxOutput int
}

// jobWriter funnels the running command's merged output into the job buffer
// under the lock so a concurrent poll always sees a consistent prefix.
type jobWriter struct{ j *job }

func (w jobWriter) Write(p []byte) (int, error) {
	w.j.mu.Lock()
	limit := w.j.maxOutput
	if limit <= 0 {
		limit = limits.MaxJobOutputBytes
	}
	remaining := limit - len(w.j.out)
	if remaining > len(p) {
		remaining = len(p)
	}
	if remaining > 0 {
		w.j.out = append(w.j.out, p[:remaining]...)
	}
	if remaining < len(p) {
		if limit >= len(jobOutputTruncated) {
			copy(w.j.out[limit-len(jobOutputTruncated):], jobOutputTruncated)
		}
	}
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
	mu      sync.Mutex
	jobs    map[string]*job
	running int
	limits  jobLimits
	onDone  func(command, cwd string, code int)
}

func newJobStore() *jobStore { return newJobStoreWithLimits(defaultJobLimits()) }

func newJobStoreWithLimits(l jobLimits) *jobStore {
	return &jobStore{jobs: map[string]*job{}, limits: l}
}

func (s *jobStore) get(id string) *job {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
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
	j := &job{id: hex.EncodeToString(idb), cmd: command, started: time.Now(), maxOutput: s.limits.maxOutput}

	s.mu.Lock()
	s.gcLocked()
	if s.running >= s.limits.maxConcurrent {
		s.mu.Unlock()
		return "", errTooManyJobs
	}
	s.jobs[j.id] = j
	s.running++
	s.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), s.limits.runTimeout)
		code, err := server.RunOneShotContext(ctx, shell, command, cwd, jobWriter{j})
		ctxErr := ctx.Err()
		cancel()
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			err = fmt.Errorf("job timed out after %s", s.limits.runTimeout)
			code = -1
		}
		if err != nil {
			jobWriter{j}.Write([]byte("\n[wanctl: job error: " + err.Error() + "]\n"))
			code = -1
		}
		j.mu.Lock()
		j.done = true
		j.code = code
		j.finished = time.Now()
		j.mu.Unlock()
		if s.onDone != nil {
			s.onDone(command, cwd, code)
		}

		s.mu.Lock()
		s.running--
		s.gcLocked()
		s.mu.Unlock()
	}()
	return j.id, nil
}

// gcLocked drops expired jobs and evicts the oldest completed jobs until both
// retained job-count and retained-output-byte budgets are satisfied.
func (s *jobStore) gcLocked() {
	cutoff := time.Now().Add(-s.limits.retain)
	type retainedJob struct {
		id       string
		finished time.Time
		bytes    int
	}
	var retained []retainedJob
	retainedBytes := 0
	for id, j := range s.jobs {
		j.mu.Lock()
		expired := j.done && j.finished.Before(cutoff)
		done, finished, outputBytes := j.done, j.finished, len(j.out)
		j.mu.Unlock()
		if expired {
			delete(s.jobs, id)
			continue
		}
		if done {
			retained = append(retained, retainedJob{id: id, finished: finished, bytes: outputBytes})
			retainedBytes += outputBytes
		}
	}
	sort.Slice(retained, func(i, j int) bool { return retained[i].finished.Before(retained[j].finished) })
	for len(retained) > s.limits.maxRetained || retainedBytes > s.limits.maxBytes {
		oldest := retained[0]
		retained = retained[1:]
		delete(s.jobs, oldest.id)
		retainedBytes -= oldest.bytes
	}
}
