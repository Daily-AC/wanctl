package server

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
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
// Nothing here is allowed to fail the command, and nothing here is allowed to
// lie about what it kept. A file that could not be created or written is
// abandoned on the first error rather than retried per chunk, and Close then
// reports no path at all — a truncated file advertised as the complete output
// is worse than no file, because the caller would grep it and believe the
// absence of a match.
type SpillWriter struct {
	w         io.Writer
	threshold int64
	total     int64
	kept      int64
	held      []byte // output so far, kept until the threshold is crossed
	file      *os.File
	path      string
	failed    bool // an error was hit; this spill is over and reports nothing
	full      bool // the size cap was reached; the file holds a prefix
}

// spillPrefix names the files this writes, so the cleanup below can recognise
// its own and nothing else.
const spillPrefix = "wanctl-exec-"

const (
	// SpillRetention is how long a spilled output file stays on the device. It
	// matches the hour a finished background job stays pollable: both are "long
	// enough for the caller to come back and look", and neither is storage.
	SpillRetention = time.Hour

	// MaxSpillBytes caps one spill file. A command can emit gigabytes, and a
	// device whose agent is the only thing keeping it reachable must not fill
	// its disk to be helpful. The file keeps the FIRST 8 MiB; the controller is
	// already showing the last of it, so between them the two ends are covered
	// and the caller is told the middle is gone.
	MaxSpillBytes = 8 << 20

	// MaxSpillFiles caps how many spills are retained at once, oldest evicted
	// first. Retention alone bounds nothing over an hour of busy driving.
	MaxSpillFiles = 32
)

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
	// Counting continues whatever happens to the file: the true length of the
	// output is what tells the controller its answer is a tail, and that is
	// worth saying even when nothing could be kept.
	s.total += int64(len(p))
	switch {
	case s.failed || s.full:
		// Decided already. No file to write to, and nothing to reconsider per
		// chunk — retrying an open here would put a directory scan between the
		// device and every block of a command's output.
	case s.file != nil:
		s.append(p)
	case s.total > s.threshold:
		s.open()
		if s.file != nil {
			s.append(s.held)
			s.append(p)
		}
		s.held = nil
	default:
		s.held = append(s.held, p...)
	}
	return n, err
}

// append writes to the spill file, stopping for good at the size cap and
// abandoning the spill on the first write error.
func (s *SpillWriter) append(p []byte) {
	if room := int64(MaxSpillBytes) - s.kept; int64(len(p)) > room {
		p = p[:max(room, 0)]
		s.full = true
	}
	if len(p) == 0 {
		return
	}
	n, err := s.file.Write(p)
	s.kept += int64(n)
	if err != nil {
		s.abandon()
	}
}

// abandon gives up on this spill: the partial file is removed and Close will
// report no path, so nothing downstream can mistake it for the whole output.
func (s *SpillWriter) abandon() {
	s.failed = true
	if s.file != nil {
		s.file.Close()
		s.file = nil
	}
	if s.path != "" {
		os.Remove(s.path)
		s.path = ""
	}
	s.held, s.kept = nil, 0
}

// open creates the spill file, first sweeping away the ones that have expired
// or are surplus to the retained count. The sweep happens here rather than on a
// timer because the agent has no other reason to wake up, and the only device
// that accumulates these files is one that is being driven right now.
func (s *SpillWriter) open() {
	SweepSpills()
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		s.failed = true
		return
	}
	path := filepath.Join(os.TempDir(), spillPrefix+hex.EncodeToString(random[:])+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		// One attempt. A device that cannot open a file in its temp dir will
		// not be able to a millisecond later either, and the command's own
		// output must not wait on finding out.
		s.failed = true
		return
	}
	s.file, s.path = f, path
}

// Close finishes the spill and reports where the whole output went — "" when
// nothing was kept — together with how many bytes passed through and how many
// of them the file actually holds.
//
// total is reported even when path is empty, and that is the difference between
// the two silences a controller has to tell apart: a device that tried and could
// not keep the output counted it, and an agent too old to know the request at
// all reports nothing.
func (s *SpillWriter) Close() (path string, total, kept int64) {
	if s.file != nil {
		s.file.Close()
		s.file = nil
	}
	s.held = nil
	if s.failed {
		return "", s.total, 0
	}
	return s.path, s.total, s.kept
}

// SweepSpills removes spill files that have expired or are surplus to
// MaxSpillFiles, oldest first. The agent calls it at startup, so a device that
// was driven hard and then restarted does not carry the whole pile forward.
func SweepSpills() { sweepSpills(os.TempDir(), time.Now()) }

// sweepSpills is SweepSpills against a named directory and clock. Best effort
// throughout: a file that cannot be stat'd or removed is left alone.
func sweepSpills(dir string, now time.Time) {
	matches, err := filepath.Glob(filepath.Join(dir, spillPrefix+"*.log"))
	if err != nil {
		return
	}
	type spillFile struct {
		path string
		mod  time.Time
	}
	var live []spillFile
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > SpillRetention {
			os.Remove(m)
			continue
		}
		live = append(live, spillFile{path: m, mod: info.ModTime()})
	}
	if len(live) < MaxSpillFiles {
		return
	}
	// One is about to be created, so make room for it as well as trimming to
	// the cap: the count after this call is what the cap is about.
	sort.Slice(live, func(i, j int) bool { return live[i].mod.Before(live[j].mod) })
	for i := 0; i <= len(live)-MaxSpillFiles; i++ {
		os.Remove(live[i].path)
	}
}
