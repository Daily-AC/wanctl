package server

import (
	"bytes"
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
	path, total := s.Close()

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
	path, total := s.Close()
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
	if path, _ := s.Close(); path != "" {
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
