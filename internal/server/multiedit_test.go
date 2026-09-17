package server

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"

	"wanctl/internal/protocol"
)

// The point of a batch is that the bytes outside the edits are untouched and
// every `old` means what it meant in the file the caller read. Three edits
// spread across a file, in one call, and the rest of it identical afterwards.
func TestMultiEditAppliesEveryEntryAndTouchesNothingElse(t *testing.T) {
	root := t.TempDir()
	const original = "alpha = 1\nbeta = 2\n# keep me\ngamma = 3\ndelta = 4\n"
	path := writeFile(t, root, "app.conf", original, 0o644)

	got := reply(t, root, protocol.Message{
		Kind: protocol.KindFileEdit, Path: path,
		Edits: []protocol.FileEdit{
			{Old: "alpha = 1", New: "alpha = 10"},
			{Old: "gamma = 3", New: "gamma = 30"},
			{Old: "delta = 4", New: "delta = 40"},
		},
	})
	if got.Kind != protocol.KindFileResult {
		t.Fatalf("reply = %q %s", got.Kind, got.Reason)
	}
	if got.File.Replaced != 3 {
		t.Errorf("replaced = %d, want 3", got.File.Replaced)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const want = "alpha = 10\nbeta = 2\n# keep me\ngamma = 30\ndelta = 40\n"
	if string(b) != want {
		t.Fatalf("file =\n%q\nwant\n%q", b, want)
	}
	if got.File.SHA256 != fileSHA(t, path) {
		t.Errorf("reported sha256 %s does not match the file", got.File.SHA256)
	}
}

// Entries match the ORIGINAL text, not the result of the entry before them. An
// implementation that applied them one after another would find the second
// `old` gone, or would match text the first edit had just created.
func TestMultiEditMatchesTheOriginalNotTheRunningResult(t *testing.T) {
	root := t.TempDir()
	path := writeFile(t, root, "swap.txt", "first: A\nsecond: B\n", 0o644)

	got := reply(t, root, protocol.Message{
		Kind: protocol.KindFileEdit, Path: path,
		Edits: []protocol.FileEdit{
			{Old: "first: A", New: "first: B"},
			{Old: "second: B", New: "second: A"},
		},
	})
	if got.Kind != protocol.KindFileResult {
		t.Fatalf("reply = %q %s", got.Kind, got.Reason)
	}
	if b, _ := os.ReadFile(path); string(b) != "first: B\nsecond: A\n" {
		t.Fatalf("file = %q", b)
	}
}

// A refusal names the entry to fix. "edits is invalid" tells a caller holding
// six of them nothing, and the file is left exactly as it was either way.
func TestMultiEditRefusesByIndexAndWritesNothing(t *testing.T) {
	const original = "x = 1\ny = 1\nz = 2\n"
	for _, tc := range []struct {
		name  string
		edits []protocol.FileEdit
		want  string
	}{
		{
			"an old that matches twice",
			[]protocol.FileEdit{{Old: "z = 2", New: "z = 3"}, {Old: "= 1", New: "= 9"}},
			"edits[1]: old string occurs 2 times",
		},
		{
			"an old that matches nothing",
			[]protocol.FileEdit{{Old: "x = 1", New: "x = 2"}, {Old: "nowhere", New: "here"}},
			"edits[1]: old string not found",
		},
		{
			"two entries claiming the same bytes",
			[]protocol.FileEdit{{Old: "x = 1\ny = 1", New: "x = 2\ny = 2"}, {Old: "y = 1\nz = 2", New: "y = 3\nz = 3"}},
			"edits[1] overlaps edits[0]",
		},
		{
			"an empty old",
			[]protocol.FileEdit{{Old: "", New: "nope"}},
			"edits[0]: 'old' must not be empty",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := writeFile(t, root, "app.conf", original, 0o644)
			before := fileSHA(t, path)

			got := reply(t, root, protocol.Message{
				Kind: protocol.KindFileEdit, Path: path, Edits: tc.edits,
			})
			if got.Kind != protocol.KindError {
				t.Fatalf("reply = %q, want an error", got.Kind)
			}
			if !strings.Contains(got.Reason, tc.want) {
				t.Errorf("refusal = %q, want it to contain %q", got.Reason, tc.want)
			}
			if fileSHA(t, path) != before {
				t.Error("the refused batch changed the file")
			}
			if b, _ := os.ReadFile(path); string(b) != original {
				t.Errorf("file = %q", b)
			}
		})
	}
}

// The two forms are exclusive. A request carrying both has two answers, and
// picking one silently is how the wrong text ends up in the file.
func TestMultiEditRefusesMixingTheTwoForms(t *testing.T) {
	root := t.TempDir()
	path := writeFile(t, root, "app.conf", "a = 1\n", 0o644)

	got := reply(t, root, protocol.Message{
		Kind: protocol.KindFileEdit, Path: path,
		Old: "a = 1", New: "a = 2",
		Edits: []protocol.FileEdit{{Old: "a = 1", New: "a = 3"}},
	})
	if got.Kind != protocol.KindError {
		t.Fatalf("reply = %q, want an error", got.Kind)
	}
	if !strings.Contains(got.Reason, "not both") {
		t.Errorf("refusal = %q", got.Reason)
	}
	if b, _ := os.ReadFile(path); string(b) != "a = 1\n" {
		t.Errorf("file = %q", b)
	}
}

// `all` means "replace every occurrence", which is exactly what an entry of a
// batch may not do. Accepting the combination would make one entry silently
// broader than the caller could see.
func TestMultiEditRefusesAll(t *testing.T) {
	root := t.TempDir()
	path := writeFile(t, root, "app.conf", "a = 1\n", 0o644)

	got := reply(t, root, protocol.Message{
		Kind: protocol.KindFileEdit, Path: path, All: true,
		Edits: []protocol.FileEdit{{Old: "a = 1", New: "a = 2"}},
	})
	if got.Kind != protocol.KindError || !strings.Contains(got.Reason, "'all' applies only") {
		t.Fatalf("reply = %q %s", got.Kind, got.Reason)
	}
}

// A stale hash refuses the batch and hands back the file's current one, so the
// caller can re-read and redo rather than guess what changed.
func TestMultiEditHonoursExpectedSHA(t *testing.T) {
	root := t.TempDir()
	path := writeFile(t, root, "app.conf", "a = 1\nb = 2\n", 0o644)
	current := fileSHA(t, path)
	stale := strings.Repeat("0", 64)

	got := reply(t, root, protocol.Message{
		Kind: protocol.KindFileEdit, Path: path, ExpectedSHA: stale,
		Edits: []protocol.FileEdit{{Old: "a = 1", New: "a = 2"}},
	})
	if got.Kind != protocol.KindError {
		t.Fatalf("reply = %q, want an error", got.Kind)
	}
	if !strings.Contains(got.Reason, "changed since it was read") || !strings.Contains(got.Reason, current) {
		t.Errorf("refusal should carry the current sha: %s", got.Reason)
	}
	if got.File == nil || got.File.SHA256 != current {
		t.Error("the refusal did not carry the current hash as data")
	}
	if b, _ := os.ReadFile(path); string(b) != "a = 1\nb = 2\n" {
		t.Errorf("file = %q", b)
	}
}

// Line endings are the file's, not the edit's. A CRLF file patched through this
// stays CRLF everywhere the edits did not touch, and inside them too.
func TestMultiEditPreservesCRLF(t *testing.T) {
	root := t.TempDir()
	const original = "one = 1\r\ntwo = 2\r\nthree = 3\r\n"
	path := writeFile(t, root, "win.conf", original, 0o644)

	got := reply(t, root, protocol.Message{
		Kind: protocol.KindFileEdit, Path: path,
		Edits: []protocol.FileEdit{
			{Old: "one = 1", New: "one = 11"},
			{Old: "three = 3", New: "three = 33"},
		},
	})
	if got.Kind != protocol.KindFileResult {
		t.Fatalf("reply = %q %s", got.Kind, got.Reason)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "one = 11\r\ntwo = 2\r\nthree = 33\r\n" {
		t.Fatalf("file = %q", b)
	}
	if strings.Contains(strings.ReplaceAll(string(b), "\r\n", ""), "\n") {
		t.Error("a bare LF appeared in a CRLF file")
	}
}

// The single-pair form keeps working exactly as it did, including `all`.
func TestSingleEditStillWorks(t *testing.T) {
	root := t.TempDir()
	path := writeFile(t, root, "app.conf", "a = 1\na = 1\n", 0o644)

	got := reply(t, root, protocol.Message{
		Kind: protocol.KindFileEdit, Path: path, Old: "a = 1", New: "a = 2", All: true,
	})
	if got.Kind != protocol.KindFileResult {
		t.Fatalf("reply = %q %s", got.Kind, got.Reason)
	}
	if got.File.Replaced != 2 {
		t.Errorf("replaced = %d, want 2", got.File.Replaced)
	}
	if b, _ := os.ReadFile(path); string(b) != "a = 2\na = 2\n" {
		t.Errorf("file = %q", b)
	}
}

// editCapped runs one file_edit against a scratch root with a small size cap, so
// the projected-size refusal can be provoked without building an 8 MiB fixture.
func editCapped(t *testing.T, root string, m protocol.Message, maxSize int64) protocol.Message {
	t.Helper()
	var buf bytes.Buffer
	handleFileEdit(&buf, m, root, maxSize)
	got, err := protocol.ReadMessage(&buf)
	if err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	return got
}

// A batch that would grow the file past the limit is refused BEFORE the new
// text is built. Building it first would allocate the whole oversized result on
// a device whose agent may be the only thing keeping it reachable, and only then
// discover it was too big — which is why the single-edit path has always sized
// the result first, and why this one now does too.
func TestMultiEditRefusesAGrowthOverTheLimitWithoutBuildingIt(t *testing.T) {
	root := t.TempDir()
	path := writeFile(t, root, "app.conf", "a = 1\nb = 2\n", 0o644)
	before := fileSHA(t, path)

	got := editCapped(t, root, protocol.Message{
		Kind: protocol.KindFileEdit, Path: path,
		Edits: []protocol.FileEdit{
			{Old: "a = 1", New: "a = " + strings.Repeat("1", 200)},
			{Old: "b = 2", New: "b = " + strings.Repeat("2", 200)},
		},
	}, 64)
	if got.Kind != protocol.KindError {
		t.Fatalf("reply = %q, want an error", got.Kind)
	}
	if !strings.Contains(got.Reason, "over the 64-byte edit limit") {
		t.Errorf("refusal does not name the limit: %s", got.Reason)
	}
	// The projection counts every entry, not just the first.
	// 12 bytes of file, plus 199 for each of the two entries.
	if !strings.Contains(got.Reason, "to 410 bytes") {
		t.Errorf("refusal does not report the projected size of the whole batch: %s", got.Reason)
	}
	if fileSHA(t, path) != before {
		t.Error("the refused batch changed the file")
	}
}

// Each entry is searched for across the whole file, so an unbounded list is a
// way to spend a device's CPU with one small frame. The cap is refused before
// the file is even opened.
func TestMultiEditRefusesMoreEntriesThanTheCap(t *testing.T) {
	root := t.TempDir()
	path := writeFile(t, root, "app.conf", "a = 1\n", 0o644)

	edits := make([]protocol.FileEdit, protocol.MaxBatchEdits+1)
	for i := range edits {
		edits[i] = protocol.FileEdit{Old: "a = 1", New: "a = 2"}
	}
	got := reply(t, root, protocol.Message{Kind: protocol.KindFileEdit, Path: path, Edits: edits})
	if got.Kind != protocol.KindError {
		t.Fatalf("reply = %q, want an error", got.Kind)
	}
	if !strings.Contains(got.Reason, "over the 64-entry limit") {
		t.Errorf("refusal does not name the cap: %s", got.Reason)
	}
	if b, _ := os.ReadFile(path); string(b) != "a = 1\n" {
		t.Errorf("file = %q", b)
	}

	// Exactly the cap is allowed: the limit is a ceiling, not a trap one under.
	atCap := make([]protocol.FileEdit, protocol.MaxBatchEdits)
	var sb strings.Builder
	for i := range atCap {
		fmt.Fprintf(&sb, "key%d = old\n", i)
		atCap[i] = protocol.FileEdit{Old: fmt.Sprintf("key%d = old", i), New: fmt.Sprintf("key%d = new", i)}
	}
	full := writeFile(t, root, "many.conf", sb.String(), 0o644)
	ok := reply(t, root, protocol.Message{Kind: protocol.KindFileEdit, Path: full, Edits: atCap})
	if ok.Kind != protocol.KindFileResult {
		t.Fatalf("a batch of exactly %d was refused: %s", protocol.MaxBatchEdits, ok.Reason)
	}
	if ok.File.Replaced != protocol.MaxBatchEdits {
		t.Errorf("replaced = %d, want %d", ok.File.Replaced, protocol.MaxBatchEdits)
	}
}
