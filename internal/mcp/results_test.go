package mcp

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// parseWorkspaceText reads back the lines workspaceText writes: a header of
// "key: value" lines and the request line, then a blank line and the output.
// It is the test's view of what a model reads, so a field the parser cannot
// find is a field the model was not shown.
func parseWorkspaceText(t *testing.T, text string) map[string]any {
	t.Helper()
	header, output, _ := strings.Cut(text, "\n\n")
	d := map[string]any{"done": false, "output": output}
	request := regexp.MustCompile(`^request (\S+): (done, exit (-?\d+)|running|waiting for the device owner's approval)$`)
	for _, line := range strings.Split(header, "\n") {
		if m := request.FindStringSubmatch(line); m != nil {
			d["request_id"] = m[1]
			if m[3] != "" {
				code, _ := strconv.Atoi(m[3])
				d["done"], d["code"] = true, code
			}
			continue
		}
		key, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		switch key {
		case "workspace", "root", "state", "cwd", "error":
			d[key] = value
		case "next_offset":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				t.Fatalf("next_offset %q in %q", value, text)
			}
			d[key] = n
		}
	}
	return d
}

var (
	readHeader = regexp.MustCompile(`^lines (\d+)-(\d+) of (\d+), (\d+) bytes, sha256 ([0-9a-f]{64})\n`)
	shaLine    = regexp.MustCompile(`sha256 ([0-9a-f]{64})`)
)

// checkToolResults drives the registered handlers over authenticated HTTP MCP,
// against the same real agent, shell and filesystem as the workspace test, and
// checks that the text alone carries what the next call needs: exit codes,
// job state, offsets, hashes and truncation.
func checkToolResults(t *testing.T, call func(string, map[string]any) (string, bool), target, ref, root string) {
	t.Helper()
	invoke := func(name string, args map[string]any) string {
		t.Helper()
		text, bad := call(name, args)
		if bad {
			t.Fatalf("%s: %s", name, text)
		}
		return text
	}
	invoke("wanctl_peers", map[string]any{})
	// Ordinary shell execution merges both streams on the device, and a
	// nonzero exit is a result, not a tool failure.
	text := invoke("wanctl_exec", map[string]any{"target": target, "oneshot": true, "command": "printf 'out'; printf 'err' >&2; exit 7"})
	if text != "exit: 7\nouterr\n" {
		t.Fatalf("ordinary exec: %q", text)
	}
	text = invoke("wanctl_exec", map[string]any{"target": target, "oneshot": true, "command": "true"})
	if text != "exit: 0\n(no output)\n" {
		t.Fatalf("silent exec: %q", text)
	}
	text = invoke("wanctl_exec", map[string]any{"target": target, "oneshot": true, "command": "printf '%60000s' x"})
	if !strings.Contains(text, "[output truncated: showing last") || len(text) > maxExecStream+1024 {
		t.Fatalf("ordinary exec truncation: %d bytes, %.200q", len(text), text)
	}
	if m := regexp.MustCompile(`full output at (\S+) on the device`).FindStringSubmatch(text); m != nil {
		t.Cleanup(func() { os.Remove(m[1]) })
	}
	// A gate makes a running poll deterministic even on a busy CI worker. A
	// poll waits for the job, so the gate is opened only after a poll has
	// come back still running.
	for _, workspace := range []bool{false, true} {
		gate := filepath.Join(root, "job-release")
		_ = os.Remove(gate)
		t.Cleanup(func() { _ = os.WriteFile(gate, nil, 0600) })
		command := "while [ ! -e '" + strings.ReplaceAll(gate, "'", "'\\''") + "' ]; do sleep 0.01; done; printf background"
		args := map[string]any{"target": target, "command": command}
		if workspace {
			delete(args, "target")
			args["workspace"], args["request_id"] = ref, "results-async"
		}
		started := invoke("wanctl_exec_async", args)
		poll := map[string]any{}
		if workspace {
			poll["workspace"], poll["job_id"] = ref, "results-async"
			if d := parseWorkspaceText(t, started); d["request_id"] != "results-async" || d["done"] != false {
				t.Fatalf("workspace submit: %q", started)
			}
		} else {
			id, ok := strings.CutPrefix(strings.TrimSpace(started), "job_id: ")
			if !ok {
				t.Fatalf("async submit: %q", started)
			}
			poll["target"], poll["job_id"] = target, id
		}
		text = invoke("wanctl_exec_poll", poll)
		if workspace {
			if parseWorkspaceText(t, text)["done"] != false {
				t.Fatalf("held workspace job already done: %q", text)
			}
		} else if !strings.HasPrefix(text, "state: running, next_offset: 0\n") || strings.Contains(text, "exit") {
			t.Fatalf("held job: %q", text)
		}
		if err := os.WriteFile(gate, nil, 0600); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(10 * time.Second); ; {
			text = invoke("wanctl_exec_poll", poll)
			if workspace {
				if d := parseWorkspaceText(t, text); d["done"] == true {
					if d["code"] != 0 || !strings.Contains(d["output"].(string), "background") {
						t.Fatalf("workspace async result: %q", text)
					}
					break
				}
			} else if strings.HasPrefix(text, "state: done") {
				if !strings.HasPrefix(text, "state: done, exit: 0, next_offset: 10\nbackground") {
					t.Fatalf("async result: %q", text)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("async command never completed: %q", text)
			}
		}
	}
	// Workspace output past one page comes back as its start and its end,
	// with the offset of the gap and next_offset at the end of the output.
	text = invoke("wanctl_exec", map[string]any{"workspace": ref, "request_id": "results-large", "command": "i=0; while [ $i -lt 3000 ]; do printf 'line %05d %s\\n' $i 0123456789012345678901234567890123456789; i=$((i+1)); done"})
	d := parseWorkspaceText(t, text)
	if d["done"] != true || len(text) > maxExecStream+1024 ||
		!strings.Contains(text, "line 00000") || !strings.Contains(text, "line 02999") ||
		!regexp.MustCompile(`\[\.\.\. \d+ bytes skipped; poll with offset=\d+ to read from there \.\.\.\]`).MatchString(text) {
		t.Fatalf("large workspace output: %d bytes, %.300q", len(text), text)
	}
	if _, more := d["next_offset"]; more {
		t.Fatalf("a finished, clamped result still asks for another page: %.300q", text)
	}
	// File metadata is useful to the next tool: a read's hash must be usable
	// by edit, while a stale hash must still refuse the write.
	file := filepath.Join(root, "results-file.txt")
	if text = invoke("wanctl_write", map[string]any{"target": target, "path": file, "content": ""}); !strings.HasPrefix(text, "created, sha256 ") {
		t.Fatalf("new file: %q", text)
	}
	if text = invoke("wanctl_read", map[string]any{"workspace": ref, "path": "results-file.txt"}); !strings.HasPrefix(text, "empty file, 0 bytes, sha256 ") {
		t.Fatalf("empty file: %q", text)
	}
	if text = invoke("wanctl_write", map[string]any{"workspace": ref, "path": "results-file.txt", "content": "苹果\n"}); !strings.HasPrefix(text, "overwrote, sha256 ") || !strings.HasSuffix(text, ", 7 bytes\n") {
		t.Fatalf("overwrite: %q", text)
	}
	text = invoke("wanctl_read", map[string]any{"target": target, "path": file})
	m := readHeader.FindStringSubmatch(text)
	if m == nil || m[1] != "1" || m[2] != "1" || !strings.HasSuffix(text, "--- content ---\n苹果\n") {
		t.Fatalf("read: %q", text)
	}
	sha := m[5]
	text = invoke("wanctl_edit", map[string]any{"target": target, "path": file, "old": "苹果", "new": "桃子", "expected_sha256": sha})
	if !strings.HasPrefix(text, "replaced 1, new sha256 ") || shaLine.FindStringSubmatch(text)[1] == sha {
		t.Fatalf("edit: %q", text)
	}
	text, bad := call("wanctl_edit", map[string]any{"workspace": ref, "path": "results-file.txt", "old": "桃子", "new": "wrong", "expected_sha256": sha})
	if !bad || !strings.Contains(text, "changed since") {
		t.Fatalf("stale-hash refusal: %s", text)
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "桃子\n" {
		t.Fatalf("failed edit changed file: %q %v", b, err)
	}
	// Reads page by line within one page of bytes; continuing at the named
	// offset neither loses nor repeats a line.
	large := strings.Repeat("苹果\n", 60000)
	if err := os.WriteFile(file, []byte(large), 0600); err != nil {
		t.Fatal(err)
	}
	text = invoke("wanctl_read", map[string]any{"target": target, "path": file, "limit": 100000})
	m = readHeader.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("read header: %.300q", text)
	}
	last, _ := strconv.Atoi(m[2])
	if len(text) > maxReadReturn+512 || !strings.Contains(text, "TRUNCATED at the 48 KiB cap") ||
		!strings.Contains(text, "offset="+strconv.Itoa(last+1)) || strings.Count(text, "苹果\n") != last {
		t.Fatalf("read pagination: %d bytes, %.300q", len(text), text)
	}
	text = invoke("wanctl_read", map[string]any{"target": target, "path": file, "offset": last + 1, "limit": 3})
	if m = readHeader.FindStringSubmatch(text); m == nil || m[1] != strconv.Itoa(last+1) || m[2] != strconv.Itoa(last+3) {
		t.Fatalf("continued read: %.300q", text)
	}
	// A line longer than the page is named, not paged.
	if err := os.WriteFile(file, []byte(strings.Repeat("x", 300000)), 0600); err != nil {
		t.Fatal(err)
	}
	text = invoke("wanctl_read", map[string]any{"workspace": ref, "path": "results-file.txt"})
	if !strings.Contains(text, "TRUNCATED: line 1 alone is over the 48 KiB cap") || strings.Contains(text, "offset=") || len(text) > maxReadReturn+512 {
		t.Fatalf("oversized line: %d bytes, %.300q", len(text), text)
	}
	if err := os.WriteFile(file, []byte("short\n"+strings.Repeat("y", 100000)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	text = invoke("wanctl_read", map[string]any{"workspace": ref, "path": "results-file.txt"})
	if m = readHeader.FindStringSubmatch(text); m == nil || m[2] != "1" || !strings.Contains(text, "offset=2") {
		t.Fatalf("read before an oversized line: %.300q", text)
	}
}
