package webfetch

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// call submits one operation and returns its finished job document.
func (f *liveFixture) call(t *testing.T, session string, values url.Values) map[string]any {
	t.Helper()
	status, job := fetchDoc(t, session+"/call?"+values.Encode())
	if status != 200 {
		t.Fatalf("submit %v = %d %v", values.Get("tool"), status, job)
	}
	return awaitJob(t, job)
}

func result(t *testing.T, job map[string]any) map[string]any {
	t.Helper()
	res, ok := job["result"].(map[string]any)
	if !ok {
		t.Fatalf("job carries no result: %v", job)
	}
	return res
}

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// numbered builds a file whose every line names itself, so a paging bug shows up
// as a missing or repeated number rather than as a plausible wall of text.
func numbered(lines int) string {
	var b strings.Builder
	for i := 1; i <= lines; i++ {
		fmt.Fprintf(&b, "line %d: 每行都自报行号\n", i)
	}
	return b.String()
}

// Paging is the whole point of offset/limit: a model that reads a file in
// windows must be able to put it back together byte for byte.
func TestWebFetchReadTextPagesWithoutLossOrRepeat(t *testing.T) {
	f := liveWebFetch(t)
	session, _ := f.approve(t, testTicket("0"), true)
	path := filepath.Join(f.root, "paged.txt")
	whole := numbered(120)
	write(t, path, whole)
	target := f.device.Target()

	window := url.Values{"rid": {"page-1"}, "tool": {"read_text"}, "target": {target}, "path": {path}, "offset": {"1"}, "limit": {"10"}}
	first := f.call(t, session, window)
	if first["status"] != "done" {
		t.Fatalf("read failed: %v", first)
	}
	res := result(t, first)
	if res["first_line"] != float64(1) || res["last_line"] != float64(10) || res["total_lines"] != float64(120) {
		t.Fatalf("window 1 = %v", res)
	}
	if res["sha256"] != fileSHA(t, path) {
		t.Fatalf("read reported a hash that is not the file's: %v", res["sha256"])
	}
	if res["size_bytes"] != float64(len(whole)) || res["truncated"] != false {
		t.Fatalf("window 1 misreports the file: %v", res)
	}
	if _, present := res["next_offset"]; present {
		t.Fatalf("an untruncated window named a continuation: %v", res)
	}

	// The identical URL under the same rid returns the same job, never a second
	// read; that is how a lost response is recovered.
	_, replay := fetchDoc(t, session+"/call?"+window.Encode())
	if replay["job_id"] != first["job_id"] || replay["duplicate_request"] != true {
		t.Fatalf("an identical read URL did not replay its job: %v", replay)
	}

	// Walk the file in windows of ten, continuing at last_line + 1 each time.
	var rebuilt strings.Builder
	rebuilt.WriteString(res["content"].(string))
	line := int(res["last_line"].(float64)) + 1
	for page := 2; line <= 120; page++ {
		window.Set("rid", fmt.Sprintf("page-%d", page))
		window.Set("offset", fmt.Sprint(line))
		job := f.call(t, session, window)
		if job["status"] != "done" {
			t.Fatalf("page %d failed: %v", page, job)
		}
		res = result(t, job)
		if res["first_line"] != float64(line) {
			t.Fatalf("page %d started at %v, asked for %d", page, res["first_line"], line)
		}
		rebuilt.WriteString(res["content"].(string))
		line = int(res["last_line"].(float64)) + 1
	}
	if rebuilt.String() != whole {
		t.Fatalf("paging did not reproduce the file: got %d bytes, want %d", rebuilt.Len(), len(whole))
	}

	// A file that is not UTF-8 text is refused, and the refusal says so rather
	// than handing back mangled bytes.
	binary := filepath.Join(f.root, "blob.bin")
	write(t, binary, "\x00\x01\x02 not text")
	refused := f.call(t, session, url.Values{"rid": {"binary"}, "tool": {"read_text"}, "target": {target}, "path": {binary}})
	if refused["status"] != "failed" {
		t.Fatalf("a binary file was not refused: %v", refused)
	}
	res = result(t, refused)
	if !strings.Contains(fmt.Sprint(res["error"]), "not a UTF-8 text file") {
		t.Fatalf("refusal does not name the reason: %v", res)
	}
	if res["error_code"] != "file_refused" || res["execution_started"] != false {
		t.Fatalf("a read that changed nothing did not say so: %v", res)
	}
}

// One line larger than a whole response is the case where paging on cannot make
// progress, so the result has to say that instead of naming a next offset.
func TestWebFetchReadTextNamesALineTooLargeToPage(t *testing.T) {
	f := liveWebFetch(t)
	session, _ := f.approve(t, testTicket("1"), true)
	path := filepath.Join(f.root, "wide.txt")
	huge := strings.Repeat("x", MaxOutputBytes+4096)
	write(t, path, "short first line\n"+huge+"\nshort third line\n")
	target := f.device.Target()

	job := f.call(t, session, url.Values{"rid": {"wide-2"}, "tool": {"read_text"}, "target": {target}, "path": {path}, "offset": {"2"}})
	res := result(t, job)
	if job["status"] != "done" {
		t.Fatalf("read of a wide line failed: %v", job)
	}
	if res["long_line"] != float64(2) || res["truncated"] != true {
		t.Fatalf("the oversized line was not named: %v", res)
	}
	if _, present := res["next_offset"]; present {
		t.Fatalf("a line that cannot fit was given a continuation: %v", res)
	}
	if !strings.Contains(fmt.Sprint(res["instruction"]), "exec") {
		t.Fatalf("the instruction does not send the caller to another tool: %v", res["instruction"])
	}
	if content := res["content"].(string); len(content) > MaxOutputBytes || !strings.HasPrefix(content, "xxxx") {
		t.Fatalf("content = %d bytes, prefix %.8q", len(content), content)
	}

	// The caller is not stuck: the lines on either side still read normally.
	before := result(t, f.call(t, session, url.Values{"rid": {"wide-1"}, "tool": {"read_text"}, "target": {target}, "path": {path}, "offset": {"1"}, "limit": {"1"}}))
	after := result(t, f.call(t, session, url.Values{"rid": {"wide-3"}, "tool": {"read_text"}, "target": {target}, "path": {path}, "offset": {"3"}}))
	if before["content"] != "short first line\n" || after["content"] != "short third line\n" {
		t.Fatalf("reading around the wide line broke: %v / %v", before, after)
	}
}

// An edit either applies exactly the span it was given or writes nothing, and
// every refusal has to hand back what the caller needs to correct it.
func TestWebFetchEditTextAppliesOneSpanAndRefusesTheRest(t *testing.T) {
	f := liveWebFetch(t)
	session, _ := f.approve(t, testTicket("9"), true)
	path := filepath.Join(f.root, "crlf.txt")
	original := "alpha\r\nbeta\r\nbeta\r\ngamma 汉字\r\n"
	write(t, path, original)
	target := f.device.Target()

	read := result(t, f.call(t, session, url.Values{"rid": {"edit-read"}, "tool": {"read_text"}, "target": {target}, "path": {path}}))
	sha, _ := read["sha256"].(string)
	if sha != fileSHA(t, path) {
		t.Fatalf("read hash = %v", read["sha256"])
	}

	// Happy path, with the hash the read reported.
	applied := f.call(t, session, url.Values{
		"rid": {"edit-1"}, "tool": {"edit_text"}, "target": {target}, "path": {path},
		"old": {"alpha"}, "new": {"ALPHA"}, "expected_sha256": {sha},
	})
	if applied["status"] != "done" {
		t.Fatalf("edit failed: %v", applied)
	}
	res := result(t, applied)
	want := "ALPHA\r\nbeta\r\nbeta\r\ngamma 汉字\r\n"
	if res["replaced"] != float64(1) || res["sha256"] != fileSHA(t, path) || res["size_bytes"] != float64(len(want)) {
		t.Fatalf("edit result = %v", res)
	}
	if data, _ := os.ReadFile(path); string(data) != want {
		t.Fatalf("bytes outside the span changed: %q", data)
	}

	// Ambiguity is refused with the count, not resolved by guessing.
	ambiguous := f.call(t, session, url.Values{
		"rid": {"edit-2"}, "tool": {"edit_text"}, "target": {target}, "path": {path},
		"old": {"beta"}, "new": {"BETA"},
	})
	if ambiguous["status"] != "failed" {
		t.Fatalf("an ambiguous edit was applied: %v", ambiguous)
	}
	res = result(t, ambiguous)
	if res["occurrences"] != float64(2) || !strings.Contains(fmt.Sprint(res["error"]), "occurs 2 times") {
		t.Fatalf("the refusal does not carry the count: %v", res)
	}
	if res["error_code"] != "file_refused" || res["execution_started"] != false {
		t.Fatalf("a refusal that wrote nothing did not say so: %v", res)
	}
	if !strings.Contains(fmt.Sprint(res["instruction"]), "NEW rid") {
		t.Fatalf("the refusal leaves the caller no way forward: %v", res["instruction"])
	}
	if data, _ := os.ReadFile(path); string(data) != want {
		t.Fatalf("a refused edit touched the file: %q", data)
	}

	// A stale hash is refused, and the refusal carries the file's actual one.
	stale := strings.Repeat("0", 64)
	mismatch := f.call(t, session, url.Values{
		"rid": {"edit-3"}, "tool": {"edit_text"}, "target": {target}, "path": {path},
		"old": {"gamma"}, "new": {"GAMMA"}, "expected_sha256": {stale},
	})
	if mismatch["status"] != "failed" {
		t.Fatalf("a stale hash was accepted: %v", mismatch)
	}
	res = result(t, mismatch)
	current := fileSHA(t, path)
	if res["sha256"] != current || !strings.Contains(fmt.Sprint(res["error"]), current) {
		t.Fatalf("the mismatch does not hand back the actual sha %s: %v", current, res)
	}
	if data, _ := os.ReadFile(path); string(data) != want {
		t.Fatalf("a refused edit touched the file: %q", data)
	}

	// all=true is the explicit way to mean every occurrence.
	every := f.call(t, session, url.Values{
		"rid": {"edit-4"}, "tool": {"edit_text"}, "target": {target}, "path": {path},
		"old": {"beta"}, "new": {"BETA"}, "all": {"true"},
	})
	if every["status"] != "done" {
		t.Fatalf("all=true failed: %v", every)
	}
	if result(t, every)["replaced"] != float64(2) {
		t.Fatalf("all=true replaced %v", result(t, every)["replaced"])
	}
	final := "ALPHA\r\nBETA\r\nBETA\r\ngamma 汉字\r\n"
	if data, _ := os.ReadFile(path); string(data) != final {
		t.Fatalf("final file = %q", data)
	}

	// An empty replacement deletes, and it has to be sent explicitly.
	deleted := f.call(t, session, url.Values{
		"rid": {"edit-5"}, "tool": {"edit_text"}, "target": {target}, "path": {path},
		"old": {"gamma 汉字\r\n"}, "new": {""},
	})
	if deleted["status"] != "done" || result(t, deleted)["replaced"] != float64(1) {
		t.Fatalf("an explicit empty replacement failed: %v", deleted)
	}
	if data, _ := os.ReadFile(path); string(data) != "ALPHA\r\nBETA\r\nBETA\r\n" {
		t.Fatalf("delete produced %q", data)
	}
	status, missing := fetchDoc(t, session+"/call?"+url.Values{
		"rid": {"edit-6"}, "tool": {"edit_text"}, "target": {target}, "path": {path}, "old": {"ALPHA"},
	}.Encode())
	if status != 400 || !strings.Contains(fmt.Sprint(missing["error"]), "new is required") {
		t.Fatalf("an omitted replacement was accepted: %d %v", status, missing)
	}
}

// An edit rewrites the file, so the grant it needs is the write grant. A device
// that has allowed reads and nothing else must put an edit exactly where it
// puts a write.
func TestWebFetchEditIsGatedAsAWriteNotAsARead(t *testing.T) {
	f := liveWebFetch(t)
	// A directory the owner has approved nothing for. The device is in normal
	// mode, so every operation here goes to the approval gate; this fixture is
	// headless, so the gate's own answer to an unattended request is a denial.
	unruled := t.TempDir()
	path := filepath.Join(unruled, "notes.txt")
	write(t, path, "one\ntwo\n")

	session, _ := f.approve(t, testTicket("2"), true)
	target := f.device.Target()
	answers := map[string]string{}
	for _, tool := range []string{"read_text", "write_text", "edit_text"} {
		values := url.Values{"rid": {"gate-" + tool}, "tool": {tool}, "target": {target}, "path": {path}}
		switch tool {
		case "write_text":
			values.Set("content", "three\n")
		case "edit_text":
			values.Set("old", "one")
			values.Set("new", "ONE")
		}
		job := f.call(t, session, values)
		if job["status"] != "failed" {
			t.Fatalf("%s ran on a path with no rule: %v", tool, job)
		}
		answers[tool] = fmt.Sprint(result(t, job)["error"])
	}
	// The two tools that rewrite the file ask for the same grant and get the
	// same answer, word for word. A read asks for a different one.
	if answers["edit_text"] != answers["write_text"] {
		t.Fatalf("edit and write took different paths: %q vs %q", answers["edit_text"], answers["write_text"])
	}
	if !strings.Contains(answers["edit_text"], "write denied by device policy") {
		t.Fatalf("edit_text was not gated as a write: %q", answers["edit_text"])
	}
	if !strings.Contains(answers["read_text"], "read denied by device policy") {
		t.Fatalf("read_text was not gated as a read: %q", answers["read_text"])
	}
	if data, _ := os.ReadFile(path); string(data) != "one\ntwo\n" {
		t.Fatalf("a denied edit changed the file: %q", data)
	}
	// The denial belongs to the device, not to the adapter: the same edit
	// applies where the owner did approve writes.
	allowed := filepath.Join(f.root, "writable.txt")
	write(t, allowed, "one\ntwo\n")
	ok := f.call(t, session, url.Values{"rid": {"rw-edit"}, "tool": {"edit_text"}, "target": {target}, "path": {allowed}, "old": {"one"}, "new": {"ONE"}})
	if ok["status"] != "done" {
		t.Fatalf("an approved directory refused an edit: %v", ok)
	}
	if data, _ := os.ReadFile(allowed); string(data) != "ONE\ntwo\n" {
		t.Fatalf("the approved edit did not apply: %q", data)
	}
}
