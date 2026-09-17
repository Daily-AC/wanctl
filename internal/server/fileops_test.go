package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wanctl/internal/protocol"
)

// reply runs one file_read/file_edit against a scratch root and decodes the
// single control message the handler writes back. The handlers only ever write
// to the connection, so a buffer stands in for it.
func reply(t *testing.T, root string, m protocol.Message) protocol.Message {
	t.Helper()
	var buf bytes.Buffer
	switch m.Kind {
	case protocol.KindFileRead:
		handleFileRead(&buf, m, root)
	case protocol.KindFileEdit:
		handleFileEdit(&buf, m, root)
	default:
		t.Fatalf("unsupported kind %q", m.Kind)
	}
	got, err := protocol.ReadMessage(&buf)
	if err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	return got
}

func writeFile(t *testing.T, dir, name, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// A read addresses a line range, and says which lines it actually returned. The
// numbers matter more than the text: a caller paging through a file navigates by
// them, and an edit built on a misnumbered read patches the wrong place.
func TestReadReturnsTheRequestedLineRange(t *testing.T) {
	root := t.TempDir()
	var sb strings.Builder
	for i := 1; i <= 5000; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	path := writeFile(t, root, "big.txt", sb.String(), 0o644)

	got := reply(t, root, protocol.Message{Kind: protocol.KindFileRead, Path: path, Offset: 4990, Limit: 20})
	if got.Kind != protocol.KindFileResult {
		t.Fatalf("reply = %q %s", got.Kind, got.Reason)
	}
	res := got.File
	if res.FirstLine != 4990 || res.LastLine != 5000 {
		t.Fatalf("returned lines %d-%d, want 4990-5000", res.FirstLine, res.LastLine)
	}
	if res.TotalLines != 5000 {
		t.Fatalf("total_lines = %d, want 5000", res.TotalLines)
	}
	if res.Truncated {
		t.Fatal("11 short lines were reported as truncated")
	}
	want := ""
	for i := 4990; i <= 5000; i++ {
		want += fmt.Sprintf("line %d\n", i)
	}
	if res.Content != want {
		t.Fatalf("content = %q, want %q", res.Content, want)
	}
	if res.SHA256 != fileSHA(t, path) {
		t.Fatal("sha256 is not the hash of the whole file")
	}
}

// The line budget is not the only budget. A file whose lines are enormous would
// otherwise return megabytes for a request that named no limit at all, so the
// byte cap cuts the range and the reply says it did.
func TestReadStopsAtTheByteCap(t *testing.T) {
	root := t.TempDir()
	line := strings.Repeat("x", 256<<10)
	path := writeFile(t, root, "wide.txt", line+"\n"+line+"\n"+line+"\n"+line+"\n", 0o644)

	got := reply(t, root, protocol.Message{Kind: protocol.KindFileRead, Path: path})
	if got.Kind != protocol.KindFileResult {
		t.Fatalf("reply = %q %s", got.Kind, got.Reason)
	}
	if !got.File.Truncated {
		t.Fatal("a 1 MiB read was not reported as truncated")
	}
	if len(got.File.Content) > protocol.MaxReadBytes {
		t.Fatalf("returned %d bytes, over the %d-byte cap", len(got.File.Content), protocol.MaxReadBytes)
	}
	if got.File.TotalLines != 4 {
		t.Fatalf("total_lines = %d, want 4", got.File.TotalLines)
	}
}

// Returning a binary file as a JSON string would hand the caller mojibake it
// cannot tell from the real contents. Refuse, and name the two tools that do
// handle bytes.
func TestReadRefusesBinaryFiles(t *testing.T) {
	root := t.TempDir()
	path := writeFile(t, root, "blob.bin", "MZ\x00\x00binary\x00payload", 0o644)

	got := reply(t, root, protocol.Message{Kind: protocol.KindFileRead, Path: path})
	if got.Kind != protocol.KindError {
		t.Fatalf("binary file was read: %q", got.Kind)
	}
	if !strings.Contains(got.Reason, "not a UTF-8 text file") {
		t.Fatalf("reason = %q, want the not-text refusal", got.Reason)
	}
	if !strings.Contains(got.Reason, "pull") || !strings.Contains(got.Reason, "exec") {
		t.Fatalf("refusal does not say what to use instead: %q", got.Reason)
	}
}

// An ambiguous edit is the dangerous one: the caller meant one of the matches
// and would get whichever came first. Refuse, say how many there are, and leave
// the file exactly as it was.
func TestEditRefusesAmbiguousMatch(t *testing.T) {
	root := t.TempDir()
	const content = "alpha\nbeta\nalpha\n"
	path := writeFile(t, root, "dup.txt", content, 0o644)
	before := fileSHA(t, path)

	got := reply(t, root, protocol.Message{Kind: protocol.KindFileEdit, Path: path, Old: "alpha", New: "gamma"})
	if got.Kind != protocol.KindError {
		t.Fatalf("ambiguous edit was applied: %q", got.Kind)
	}
	if got.File == nil || got.File.Occurrences != 2 {
		t.Fatalf("refusal did not report 2 occurrences: %+v", got.File)
	}
	if !strings.Contains(got.Reason, "occurs 2 times") {
		t.Fatalf("reason = %q", got.Reason)
	}
	if b, _ := os.ReadFile(path); string(b) != content {
		t.Fatalf("refused edit changed the file to %q", b)
	}
	if fileSHA(t, path) != before {
		t.Fatal("refused edit changed the file's hash")
	}
}

// expected_sha256 is the whole reason an edit is safer than a whole-file push:
// it turns "someone else wrote to this file since I read it" from silent data
// loss into a refusal that hands back the current hash to re-read from.
func TestEditRefusesOnHashMismatch(t *testing.T) {
	root := t.TempDir()
	const content = "one\ntwo\n"
	path := writeFile(t, root, "raced.txt", content, 0o644)
	actual := fileSHA(t, path)

	got := reply(t, root, protocol.Message{
		Kind: protocol.KindFileEdit, Path: path, Old: "two", New: "three",
		ExpectedSHA: strings.Repeat("0", 64),
	})
	if got.Kind != protocol.KindError {
		t.Fatalf("stale edit was applied: %q", got.Kind)
	}
	if got.File == nil || got.File.SHA256 != actual {
		t.Fatalf("refusal did not carry the file's current sha256: %+v", got.File)
	}
	if !strings.Contains(got.Reason, actual) {
		t.Fatalf("reason does not name the current hash: %q", got.Reason)
	}
	if b, _ := os.ReadFile(path); string(b) != content {
		t.Fatalf("refused edit changed the file to %q", b)
	}
}

// Everything outside the replaced span is preserved byte for byte, including
// the line endings and the mode. A Windows device's CRLF file must not come
// back as LF because the edit went through something that normalized it.
func TestEditPreservesBytesAndMode(t *testing.T) {
	root := t.TempDir()
	const content = "first\r\nsecond\r\nthird\r\n"
	path := writeFile(t, root, "crlf.txt", content, 0o755)

	got := reply(t, root, protocol.Message{Kind: protocol.KindFileEdit, Path: path, Old: "second", New: "SECOND"})
	if got.Kind != protocol.KindFileResult {
		t.Fatalf("edit failed: %q %s", got.Kind, got.Reason)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(content, "second", "SECOND", 1)
	if string(after) != want {
		t.Fatalf("file = %q, want %q (only the replaced span may differ)", after, want)
	}
	if bytes.Count(after, []byte("\r\n")) != 3 {
		t.Fatalf("CRLF endings were rewritten: %q", after)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 0755", info.Mode().Perm())
	}
	if got.File.SizeBytes != int64(len(want)) {
		t.Fatalf("size_bytes = %d, want %d", got.File.SizeBytes, len(want))
	}
	sum := sha256.Sum256(after)
	if got.File.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("reported sha256 is not the hash of what was written")
	}
}

// all=true is the deliberate opt-in the ambiguity refusal points to, so it has
// to actually replace every occurrence and say how many that was.
func TestEditAllReplacesEveryOccurrence(t *testing.T) {
	root := t.TempDir()
	path := writeFile(t, root, "many.txt", "a\nX\nb\nX\nc\nX\n", 0o644)

	got := reply(t, root, protocol.Message{Kind: protocol.KindFileEdit, Path: path, Old: "X", New: "Y", All: true})
	if got.Kind != protocol.KindFileResult {
		t.Fatalf("edit failed: %q %s", got.Kind, got.Reason)
	}
	if got.File.Replaced != 3 {
		t.Fatalf("replaced = %d, want 3", got.File.Replaced)
	}
	if b, _ := os.ReadFile(path); string(b) != "a\nY\nb\nY\nc\nY\n" {
		t.Fatalf("file = %q", b)
	}
}

// An empty `new` is a deletion, which is a legitimate edit; an empty `old` is
// not a search at all and would match everywhere.
func TestEditDeletionAndEmptyOldRefusal(t *testing.T) {
	root := t.TempDir()
	path := writeFile(t, root, "del.txt", "keep\nDROP\nkeep\n", 0o644)

	got := reply(t, root, protocol.Message{Kind: protocol.KindFileEdit, Path: path, Old: "DROP\n", New: ""})
	if got.Kind != protocol.KindFileResult {
		t.Fatalf("deletion failed: %q %s", got.Kind, got.Reason)
	}
	if b, _ := os.ReadFile(path); string(b) != "keep\nkeep\n" {
		t.Fatalf("file = %q", b)
	}

	got = reply(t, root, protocol.Message{Kind: protocol.KindFileEdit, Path: path, Old: "", New: "x"})
	if got.Kind != protocol.KindError {
		t.Fatalf("empty old was accepted: %q", got.Kind)
	}
}

// A file with no trailing newline still has a last line, and a read that ends
// on it must not invent one.
func TestReadCountsAFinalUnterminatedLine(t *testing.T) {
	root := t.TempDir()
	path := writeFile(t, root, "noeol.txt", "one\ntwo\nthree", 0o644)

	got := reply(t, root, protocol.Message{Kind: protocol.KindFileRead, Path: path})
	if got.File.TotalLines != 3 || got.File.LastLine != 3 {
		t.Fatalf("total=%d last=%d, want 3/3", got.File.TotalLines, got.File.LastLine)
	}
	if got.File.Content != "one\ntwo\nthree" {
		t.Fatalf("content = %q", got.File.Content)
	}
}

// An offset past the end is a legitimate question with an empty answer, not an
// error: it is what paging through a file hits when it reaches the end.
func TestReadPastEndReturnsNothing(t *testing.T) {
	root := t.TempDir()
	path := writeFile(t, root, "short.txt", "one\ntwo\n", 0o644)

	got := reply(t, root, protocol.Message{Kind: protocol.KindFileRead, Path: path, Offset: 99})
	if got.Kind != protocol.KindFileResult {
		t.Fatalf("reply = %q %s", got.Kind, got.Reason)
	}
	if got.File.Content != "" || got.File.TotalLines != 2 {
		t.Fatalf("content=%q total=%d", got.File.Content, got.File.TotalLines)
	}
}

// Multi-byte text must survive both directions, including a character that
// straddles the sniff boundary used to decide the file is text at all.
func TestReadHandlesMultibyteText(t *testing.T) {
	root := t.TempDir()
	content := strings.Repeat("a", sniffLen-1) + "中文\n后面\n"
	path := writeFile(t, root, "utf8.txt", content, 0o644)

	got := reply(t, root, protocol.Message{Kind: protocol.KindFileRead, Path: path})
	if got.Kind != protocol.KindFileResult {
		t.Fatalf("UTF-8 file was refused: %q %s", got.Kind, got.Reason)
	}
	if got.File.Content != content {
		t.Fatal("multi-byte content did not round-trip")
	}
}
