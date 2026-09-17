package server

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Under the threshold nothing is kept: the controller can show the whole
// output, so a file on the device would be litter.
func TestSpillKeepsNoFileForSmallOutput(t *testing.T) {
	var sink bytes.Buffer
	s := NewSpill(&sink, 1024)
	s.Write([]byte(strings.Repeat("a", 1000)))
	path, total, _ := s.Close()

	if path != "" {
		t.Errorf("a small output was spilled to %s", path)
	}
	if total != 1000 {
		t.Errorf("total = %d, want 1000", total)
	}
	if sink.Len() != 1000 {
		t.Errorf("the controller received %d bytes, want 1000", sink.Len())
	}
}

// Past the threshold the whole output — including the part written before the
// threshold was crossed — is on the device, byte for byte.
func TestSpillKeepsTheWholeOutputOnce(t *testing.T) {
	var sink bytes.Buffer
	s := NewSpill(&sink, 1024)
	want := ""
	for i := range 40 {
		chunk := strings.Repeat(string(rune('a'+i%26)), 100)
		want += chunk
		s.Write([]byte(chunk))
	}
	path, total, _ := s.Close()
	if path == "" {
		t.Fatal("4000 bytes past a 1024-byte threshold were not spilled")
	}
	t.Cleanup(func() { os.Remove(path) })

	if total != int64(len(want)) {
		t.Errorf("total = %d, want %d", total, len(want))
	}
	if sink.String() != want {
		t.Error("the controller did not receive the output unchanged")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("the spill file holds %d bytes, want the whole %d", len(got), len(want))
	}
	if !strings.HasPrefix(filepath.Base(path), spillPrefix) || !strings.HasSuffix(path, ".log") {
		t.Errorf("spill file is named %q", filepath.Base(path))
	}
}

// A threshold of zero is what every caller that did not ask for a spill sends,
// and it must leave the device exactly as it was.
func TestSpillIsOffByDefault(t *testing.T) {
	var sink bytes.Buffer
	s := NewSpill(&sink, 0)
	s.Write(bytes.Repeat([]byte("x"), 1<<20))
	if path, _, _ := s.Close(); path != "" {
		t.Errorf("spilled to %s without being asked", path)
	}
}

// Spills are kept for an hour, not forever. The sweep runs when the next one is
// created, because that is the only moment the agent is awake for this.
func TestSweepRemovesExpiredSpillsAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, spillPrefix+"old.log")
	fresh := filepath.Join(dir, spillPrefix+"fresh.log")
	other := filepath.Join(dir, "someone-elses.log")
	for _, p := range []string{old, fresh, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * SpillRetention)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(other, past, past); err != nil {
		t.Fatal(err)
	}

	sweepSpills(dir, time.Now())

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("an expired spill survived the sweep")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("a spill inside the retention window was removed")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("the sweep removed a file that is not a spill")
	}
}

// A spill that cannot be written is abandoned, not half-kept. The danger is
// specific: a truncated file advertised as the complete output would be grepped
// by a caller who then believes the absence of a match.
func TestSpillThatCannotBeWrittenReportsNoPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	if err := os.Chmod(dir, 0o500); err != nil { // readable, not writable
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if f, err := os.CreateTemp(dir, "probe"); err == nil {
		f.Close()
		t.Skip("this filesystem ignores the directory mode (running as root?)")
	}

	var sink bytes.Buffer
	s := NewSpill(&sink, 64)
	body := strings.Repeat("x", 500)
	s.Write([]byte(body))
	path, total, kept := s.Close()

	if path != "" {
		t.Errorf("a failed spill reported %s", path)
	}
	if kept != 0 {
		t.Errorf("kept = %d, want 0 when nothing could be written", kept)
	}
	// The count survives the failure: it is what tells a controller its answer
	// is a tail, and it is how "could not keep" is told from "never asked".
	if total != int64(len(body)) {
		t.Errorf("total = %d, want %d", total, len(body))
	}
	if sink.String() != body {
		t.Error("the controller did not receive the output unchanged")
	}
}

// Once a spill has failed it stays failed: no reopen, and above all no Glob per
// chunk of a command's output.
func TestFailedSpillDoesNotRetryPerChunk(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if f, err := os.CreateTemp(dir, "probe"); err == nil {
		f.Close()
		t.Skip("this filesystem ignores the directory mode (running as root?)")
	}

	s := NewSpill(io.Discard, 8)
	for range 50 {
		s.Write([]byte("0123456789"))
	}
	if !s.failed {
		t.Fatal("the spill did not latch its failure")
	}
	if s.file != nil || s.path != "" {
		t.Error("a failed spill is still holding a file")
	}
}

// One command can emit gigabytes. The device keeps the first 8 MiB and says so,
// rather than filling the disk of the machine its agent is keeping reachable.
func TestSpillStopsAtTheSizeCap(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 9 MiB")
	}
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)

	s := NewSpill(io.Discard, 1024)
	chunk := bytes.Repeat([]byte("y"), 1<<20)
	for range 9 {
		s.Write(chunk)
	}
	path, total, kept := s.Close()
	if path == "" {
		t.Fatal("no spill file")
	}
	t.Cleanup(func() { os.Remove(path) })

	if total != 9<<20 {
		t.Errorf("total = %d, want %d", total, 9<<20)
	}
	if kept != MaxSpillBytes {
		t.Errorf("kept = %d, want the %d-byte cap", kept, MaxSpillBytes)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != MaxSpillBytes {
		t.Errorf("file is %d bytes, want the cap %d", info.Size(), MaxSpillBytes)
	}
}

// Retention alone bounds nothing over an hour of busy driving, so the count is
// capped too, oldest evicted first.
func TestSweepCapsTheNumberOfRetainedSpills(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for i := range MaxSpillFiles + 8 {
		p := filepath.Join(dir, fmt.Sprintf("%s%02d.log", spillPrefix, i))
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		// Oldest first, all well inside the retention window.
		mod := now.Add(-time.Duration(MaxSpillFiles+8-i) * time.Minute)
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}

	sweepSpills(dir, now)

	left, err := filepath.Glob(filepath.Join(dir, spillPrefix+"*.log"))
	if err != nil {
		t.Fatal(err)
	}
	// One below the cap, because the sweep runs to make room for the file about
	// to be created.
	if len(left) != MaxSpillFiles-1 {
		t.Fatalf("%d spills left, want %d", len(left), MaxSpillFiles-1)
	}
	// What survived is the newest, which is what a caller would come back for.
	for _, p := range left {
		if strings.Contains(p, spillPrefix+"00.log") || strings.Contains(p, spillPrefix+"01.log") {
			t.Errorf("the sweep kept an old spill and evicted newer ones: %s", p)
		}
	}
}
