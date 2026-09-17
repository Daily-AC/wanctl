package server

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"wanctl/internal/protocol"
)

// writeReply runs one file_write against a scratch root and decodes the single
// control message the handler writes back.
func writeReply(t *testing.T, root string, m protocol.Message, maxSize int64) protocol.Message {
	t.Helper()
	var buf bytes.Buffer
	handleFileWrite(&buf, m, root, maxSize)
	got, err := protocol.ReadMessage(&buf)
	if err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	return got
}

// A write is how a file comes into existence, so the directories on the way to
// it come into existence too. Writing the same file twice must not turn a
// second write into a create, and the mode of a brand-new file is 0644.
func TestWriteCreatesTheFileAndItsParents(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "etc", "app", "config.toml")

	got := writeReply(t, root, protocol.Message{
		Kind: protocol.KindFileWrite, Path: path, Content: "port = 8080\n",
	}, protocol.MaxEditBytes)
	if got.Kind != protocol.KindFileResult {
		t.Fatalf("reply = %q %s", got.Kind, got.Reason)
	}
	if !got.File.Created {
		t.Error("a file that did not exist was not reported as created")
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "port = 8080\n" {
		t.Fatalf("file = %q, %v", b, err)
	}
	if got.File.SizeBytes != 12 {
		t.Errorf("size = %d, want 12", got.File.SizeBytes)
	}
	if got.File.SHA256 != fileSHA(t, path) {
		t.Errorf("reported sha256 %s does not match the file", got.File.SHA256)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o644 {
			t.Errorf("new file mode = %o, want 644", info.Mode().Perm())
		}
	}

	second := writeReply(t, root, protocol.Message{
		Kind: protocol.KindFileWrite, Path: path, Content: "port = 9090\n",
	}, protocol.MaxEditBytes)
	if second.Kind != protocol.KindFileResult {
		t.Fatalf("second write: %q %s", second.Kind, second.Reason)
	}
	if second.File.Created {
		t.Error("overwriting an existing file was reported as creating it")
	}
	if b, _ := os.ReadFile(path); string(b) != "port = 9090\n" {
		t.Errorf("file = %q after overwrite", b)
	}
}

// An executable script rewritten by an agent has to stay executable. Silently
// demoting it to 0644 would break the next thing that runs it, and nothing in
// the reply would say so.
func TestWriteKeepsAnExistingFilesMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits")
	}
	root := t.TempDir()
	path := writeFile(t, root, "run.sh", "#!/bin/sh\ntrue\n", 0o755)

	got := writeReply(t, root, protocol.Message{
		Kind: protocol.KindFileWrite, Path: path, Content: "#!/bin/sh\nexec ./app\n",
	}, protocol.MaxEditBytes)
	if got.Kind != protocol.KindFileResult {
		t.Fatalf("reply = %q %s", got.Kind, got.Reason)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %o after write, want 755", info.Mode().Perm())
	}
}

// The cap is refused before anything is written, not after a partial file is
// already on disk.
func TestWriteRefusesContentOverTheLimit(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "big.txt")

	got := writeReply(t, root, protocol.Message{
		Kind: protocol.KindFileWrite, Path: path, Content: strings.Repeat("x", 1025),
	}, 1024)
	if got.Kind != protocol.KindError {
		t.Fatalf("reply = %q, want an error", got.Kind)
	}
	if !strings.Contains(got.Reason, "over the 1024-byte write limit") {
		t.Errorf("refusal does not name the limit: %s", got.Reason)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the refused write left a file behind")
	}
}

// Bytes that are not UTF-8 would not survive the JSON trip intact, so they are
// refused rather than written as U+FFFD soup, and the refusal names the tool
// that does move bytes.
func TestWriteRefusesContentThatIsNotText(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "blob.bin")

	got := writeReply(t, root, protocol.Message{
		Kind: protocol.KindFileWrite, Path: path, Content: "ok\x00then a NUL",
	}, protocol.MaxEditBytes)
	if got.Kind != protocol.KindError {
		t.Fatalf("reply = %q, want an error", got.Kind)
	}
	if !strings.Contains(got.Reason, "not a UTF-8 text file") || !strings.Contains(got.Reason, "push_blob") {
		t.Errorf("refusal should name the binary path: %s", got.Reason)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the refused write left a file behind")
	}
}

// An empty string is a legitimate write — it truncates the file — and must not
// be read as "no content given".
func TestWriteAcceptsEmptyContent(t *testing.T) {
	root := t.TempDir()
	path := writeFile(t, root, "notes.txt", "something\n", 0o644)

	got := writeReply(t, root, protocol.Message{Kind: protocol.KindFileWrite, Path: path}, protocol.MaxEditBytes)
	if got.Kind != protocol.KindFileResult {
		t.Fatalf("reply = %q %s", got.Kind, got.Reason)
	}
	if b, _ := os.ReadFile(path); len(b) != 0 {
		t.Fatalf("file = %q, want empty", b)
	}
}

// The policy root binds the write itself, not a path string checked earlier.
func TestWriteStaysInsideThePolicyRoot(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "elsewhere.txt")

	got := writeReply(t, root, protocol.Message{
		Kind: protocol.KindFileWrite, Path: outside, Content: "nope\n",
	}, protocol.MaxEditBytes)
	if got.Kind != protocol.KindError {
		t.Fatalf("reply = %q, want an error", got.Kind)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Error("a write outside the policy root landed")
	}
}
