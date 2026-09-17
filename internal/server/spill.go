package server

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"time"
)

// SpillWriter passes a command's output through to the controller and, once
// that output grows past a threshold, also keeps the whole of it in a file on
// the device.
//
// The problem it solves belongs to the caller, not to the device. An MCP tool
// result has to fit in a model's context, so a controller caps what it hands
// back; everything past the cap used to be simply gone, and a build whose error
// was in the middle of 3 MB of output could not be recovered except by running
// it again with a filter guessed blind. Keeping the full output where it was
// produced costs one file and no extra bytes over the relay: the controller
// reports a path, and the next `grep` runs on the device.
//
// Nothing here is allowed to fail the command. If the file cannot be created or
// written, the spill is abandoned, Path returns "" and the controller says the
// full output was not kept — a screenshot of a failure is worth less than the
// command's own result.
type SpillWriter struct {
	w         io.Writer
	threshold int64
	total     int64
	held      []byte // output so far, kept until the threshold is crossed
	file      *os.File
	path      string
}

// spillPrefix names the files this writes, so the cleanup below can recognise
// its own and nothing else.
const spillPrefix = "wanctl-exec-"

// SpillRetention is how long a spilled output file stays on the device. It
// matches the hour a finished background job stays pollable: both are "long
// enough for the caller to come back and look", and neither is storage.
const SpillRetention = time.Hour

// NewSpill wraps w. A threshold of zero or less disables spilling entirely,
// which is what every caller that did not ask for it gets.
func NewSpill(w io.Writer, threshold int64) *SpillWriter {
	return &SpillWriter{w: w, threshold: threshold}
}

func (s *SpillWriter) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	if s.threshold <= 0 {
		return n, err
	}
	s.total += int64(len(p))
	switch {
	case s.file != nil:
		s.file.Write(p)
	case s.total > s.threshold:
		s.open()
		if s.file != nil {
			s.file.Write(s.held)
			s.file.Write(p)
		}
		s.held = nil
	default:
		s.held = append(s.held, p...)
	}
	return n, err
}

// open creates the spill file, first sweeping away any that have expired. The
// sweep happens here rather than on a timer because the agent has no other
// reason to wake up, and the only device that accumulates these files is one
// that is being driven right now.
func (s *SpillWriter) open() {
	sweepSpills(os.TempDir(), time.Now())
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		return
	}
	path := filepath.Join(os.TempDir(), spillPrefix+hex.EncodeToString(random[:])+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	s.file, s.path = f, path
}

// Close finishes the spill and reports where the whole output went — "" when
// nothing was spilled — together with how many bytes passed through.
func (s *SpillWriter) Close() (path string, total int64) {
	if s.file != nil {
		s.file.Close()
		s.file = nil
	}
	s.held = nil
	return s.path, s.total
}

// sweepSpills removes spill files older than SpillRetention. Best effort: a
// file that cannot be stat'd or removed is left alone.
func sweepSpills(dir string, now time.Time) {
	matches, err := filepath.Glob(filepath.Join(dir, spillPrefix+"*.log"))
	if err != nil {
		return
	}
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil || now.Sub(info.ModTime()) <= SpillRetention {
			continue
		}
		os.Remove(m)
	}
}
