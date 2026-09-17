package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
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
		handleFileEdit(&buf, m, root, protocol.MaxEditBytes)
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
// byte cap cuts the range -- and it cuts on a line boundary, because the caller
// is told to continue from the line after the last one returned. A cut in the
// middle of a line would lose that line's tail or hand it back twice, and a
// model paging through a file could not tell which.
func TestReadStopsAtTheByteCapOnALineBoundary(t *testing.T) {
	root := t.TempDir()
	line := strings.Repeat("x", 100<<10) // three of these exceed the 256 KiB cap
	path := writeFile(t, root, "wide.txt", line+"\n"+line+"\n"+line+"\n"+line+"\n", 0o644)

	got := reply(t, root, protocol.Message{Kind: protocol.KindFileRead, Path: path})
	if got.Kind != protocol.KindFileResult {
		t.Fatalf("reply = %q %s", got.Kind, got.Reason)
	}
	res := got.File
	if !res.Truncated {
		t.Fatal("a 400 KiB read was not reported as truncated")
	}
	if len(res.Content) > protocol.MaxReadBytes {
		t.Fatalf("returned %d bytes, over the %d-byte cap", len(res.Content), protocol.MaxReadBytes)
	}
	if res.LongLine != 0 {
		t.Fatalf("long_line = %d, want 0: every line here fits on its own", res.LongLine)
	}
	// Two whole lines fit; the third does not, so it is not in the answer at all.
	if res.FirstLine != 1 || res.LastLine != 2 {
		t.Fatalf("returned lines %d-%d, want 1-2", res.FirstLine, res.LastLine)
	}
	if res.Content != line+"\n"+line+"\n" {
		t.Fatalf("content is not exactly two whole lines (%d bytes)", len(res.Content))
	}
	if res.TotalLines != 4 {
		t.Fatalf("total_lines = %d, want 4", res.TotalLines)
	}

	// Continuing where it said to continue returns the rest, with nothing lost
	// and nothing repeated.
	next := reply(t, root, protocol.Message{Kind: protocol.KindFileRead, Path: path, Offset: int64(res.LastLine + 1)})
	if next.File.FirstLine != 3 || next.File.Content != line+"\n"+line+"\n" {
		t.Fatalf("continuation returned lines %d-%d (%d bytes)", next.File.FirstLine, next.File.LastLine, len(next.File.Content))
	}
}

// One line larger than the entire cap is the case paging cannot solve: continue
// from the next line and you skip it, ask again and you get the same prefix
// forever. Return the prefix, name the line, and let every surface tell the
// caller to reach for a tool that can slice it.
func TestReadNamesALineTooLargeToReturn(t *testing.T) {
	root := t.TempDir()
	huge := strings.Repeat("y", 300<<10)
	path := writeFile(t, root, "onebigline.txt", "small\n"+huge+"\ntail\n", 0o644)

	got := reply(t, root, protocol.Message{Kind: protocol.KindFileRead, Path: path, Offset: 2})
	if got.Kind != protocol.KindFileResult {
		t.Fatalf("reply = %q %s", got.Kind, got.Reason)
	}
	res := got.File
	if res.LongLine != 2 {
		t.Fatalf("long_line = %d, want 2", res.LongLine)
	}
	if !res.Truncated {
		t.Fatal("an oversized line was not reported as truncated")
	}
	if len(res.Content) > protocol.MaxReadBytes || len(res.Content) == 0 {
		t.Fatalf("returned %d bytes of the long line", len(res.Content))
	}
	if !strings.HasPrefix(huge, res.Content) {
		t.Fatal("the returned prefix is not the start of that line")
	}
	if res.FirstLine != 2 || res.LastLine != 2 {
		t.Fatalf("returned lines %d-%d, want 2-2", res.FirstLine, res.LastLine)
	}
}

// The text sniff only ever sees the first 8 KiB, but what goes back is
// marshalled as a JSON string -- and encoding/json rewrites invalid UTF-8 as
// U+FFFD without saying so. A file that starts out ASCII and turns to bytes
// later would come back quietly corrupted, which is worse than a refusal.
func TestReadRefusesInvalidUTF8PastTheSniff(t *testing.T) {
	root := t.TempDir()
	content := strings.Repeat("ascii line\n", 1200) + "\xff\xfe not text\n"
	if len(content) <= sniffLen {
		t.Fatalf("test file is only %d bytes; the bad bytes must land past the %d-byte sniff", len(content), sniffLen)
	}
	path := writeFile(t, root, "turns-binary.txt", content, 0o644)

	// The prefix on its own is perfectly good text, so the sniff is happy.
	if !isText([]byte(content[:sniffLen]), int64(len(content))) {
		t.Fatal("the first 8 KiB should look like text; the test proves nothing otherwise")
	}
	got := reply(t, root, protocol.Message{Kind: protocol.KindFileRead, Path: path, Limit: 5000})
	if got.Kind != protocol.KindError || !strings.Contains(got.Reason, "not a UTF-8 text file") {
		t.Fatalf("reply = %q %q, want the not-text refusal", got.Kind, got.Reason)
	}
}

// A limit is a number the caller chooses, so MaxInt is a number the caller can
// choose. Adding it to the offset must not wrap into a last-line number smaller
// than the first.
func TestReadClampsAnAbsurdLimit(t *testing.T) {
	root := t.TempDir()
	path := writeFile(t, root, "five.txt", "1\n2\n3\n4\n5\n", 0o644)

	for _, limit := range []int{math.MaxInt, math.MaxInt - 1, 1 << 30} {
		got := reply(t, root, protocol.Message{Kind: protocol.KindFileRead, Path: path, Offset: 2, Limit: limit})
		if got.Kind != protocol.KindFileResult {
			t.Fatalf("limit %d: reply = %q %s", limit, got.Kind, got.Reason)
		}
		if got.File.FirstLine != 2 || got.File.LastLine != 5 || got.File.Content != "2\n3\n4\n5\n" {
			t.Fatalf("limit %d: returned lines %d-%d %q", limit, got.File.FirstLine, got.File.LastLine, got.File.Content)
		}
	}
}

// strings.Replace allocates its whole output in one go, so `all` with a
// replacement longer than what it replaces is a way to ask the agent for
// arbitrarily more memory than the file it was pointed at -- on a device whose
// agent may be the only thing keeping it reachable. Size the result first.
func TestEditRefusesAnEditThatWouldGrowPastTheLimit(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "wide-open.txt")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	block := bytes.Repeat([]byte("a"), 64<<10)
	for range (8 << 20) / len(block) { // exactly the 8 MiB input limit
		if _, err := f.Write(block); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()

	// 8 Mi occurrences, each growing by 1023 bytes: ~8 GiB of output.
	got := reply(t, root, protocol.Message{
		Kind: protocol.KindFileEdit, Path: path,
		Old: "a", New: strings.Repeat("b", 1024), All: true,
	})
	if got.Kind != protocol.KindError {
		t.Fatalf("an 8 GiB edit was accepted: %q", got.Kind)
	}
	if !strings.Contains(got.Reason, "would grow") || !strings.Contains(got.Reason, "nothing was written") {
		t.Fatalf("reason = %q", got.Reason)
	}
	if info, err := os.Stat(path); err != nil || info.Size() != 8<<20 {
		t.Fatalf("size after refusal = %d (err=%v), want the original 8 MiB", info.Size(), err)
	}
}

// The same rule at the boundary, cheaply: an edit that lands exactly on the cap
// is allowed and one byte more is not.
func TestEditGrowthLimitBoundary(t *testing.T) {
	root := t.TempDir()
	const editCap = 64
	for _, tt := range []struct {
		name    string
		new     string
		allowed bool
	}{
		{name: "lands on the cap", new: strings.Repeat("y", editCap), allowed: true},
		{name: "one byte over", new: strings.Repeat("y", editCap+1), allowed: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := writeFile(t, root, "boundary-"+tt.name+".txt", "xxxx", 0o644)
			var buf bytes.Buffer
			handleFileEdit(&buf, protocol.Message{Kind: protocol.KindFileEdit, Path: path, Old: "xxxx", New: tt.new}, root, editCap)
			got, err := protocol.ReadMessage(&buf)
			if err != nil {
				t.Fatal(err)
			}
			if tt.allowed && got.Kind != protocol.KindFileResult {
				t.Fatalf("an edit landing on the cap was refused: %s", got.Reason)
			}
			if !tt.allowed && got.Kind != protocol.KindError {
				t.Fatalf("an edit one byte over the cap was applied")
			}
		})
	}
}

// An edit says it works on text files, so it has to check that it is looking at
// one. Replacing a string inside a binary corrupts it exactly as surely as
// reading it back would mangle it.
func TestEditRefusesBinaryFiles(t *testing.T) {
	root := t.TempDir()
	const content = "MZ\x00\x00binary\x00payload"
	path := writeFile(t, root, "blob.bin", content, 0o644)

	got := reply(t, root, protocol.Message{Kind: protocol.KindFileEdit, Path: path, Old: "binary", New: "text"})
	if got.Kind != protocol.KindError || !strings.Contains(got.Reason, "not a UTF-8 text file") {
		t.Fatalf("reply = %q %q, want the not-text refusal", got.Kind, got.Reason)
	}
	if b, _ := os.ReadFile(path); string(b) != content {
		t.Fatalf("refused edit changed the file to %q", b)
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
