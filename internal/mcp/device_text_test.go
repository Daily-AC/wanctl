package mcp

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// toolCall calls one tool in session sid and returns what a model reads: the
// text, the structured content, and whether it is an error.
func toolCall(t *testing.T, h http.Handler, access, sid, name string, args map[string]any) (string, map[string]any, bool) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args}})
	rr, out := rpc(t, h, access, sid, string(body))
	result, _ := out["result"].(map[string]any)
	if rr.Code != http.StatusOK || result == nil {
		t.Fatalf("%s: %d %s", name, rr.Code, rr.Body.String())
	}
	var text strings.Builder
	for _, c := range result["content"].([]any) {
		if s, ok := c.(map[string]any)["text"].(string); ok {
			text.WriteString(s)
		}
	}
	data, _ := result["structuredContent"].(map[string]any)
	isError, _ := result["isError"].(bool)
	return text.String(), data, isError
}

// hasControl reports a control character other than newline, carriage return
// and tab — something a terminal would act on rather than show.
func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		return (r < 0x20 && r != '\n' && r != '\r' && r != '\t') || (r >= 0x7f && r <= 0x9f)
	})
}

// What a device says reaches the model with its control characters made
// visible: command output, polled output, the device's own name and the
// reasons it gives. A file read through wanctl_read is the exception — it
// comes back exactly as stored, which is what lets wanctl_edit match it.
func TestDeviceControlCharactersReachTheModelEscaped(t *testing.T) {
	h, access, target := hostedDeviceNamed(t, "box\x1b]0;renamed\x07")
	sid := openSession(t, h, access)
	const styled = `printf 'a\033[31mb\033[0m\tc\rd\001e\302\233f\n'`
	const shown = `a\x1b[31mb\x1b[0m` + "\tc\rd" + `\x01e\u009bf`

	text, data, _ := toolCall(t, h, access, sid, "wanctl_exec", map[string]any{"target": target, "command": styled, "oneshot": true})
	stdout, _ := data["stdout"].(string)
	for where, s := range map[string]string{"text": text, "stdout": stdout} {
		if hasControl(s) || !strings.Contains(s, shown) {
			t.Errorf("exec %s = %q, want the output with its controls escaped", where, s)
		}
	}

	_, data, _ = toolCall(t, h, access, sid, "wanctl_exec_async", map[string]any{"target": target, "command": styled})
	job, _ := data["job_id"].(string)
	for deadline := time.Now().Add(5 * time.Second); ; {
		text, data, _ = toolCall(t, h, access, sid, "wanctl_exec_poll", map[string]any{"target": target, "job_id": job})
		if data["done"] == true {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background job did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	output, _ := data["output"].(string)
	for where, s := range map[string]string{"text": text, "output": output} {
		if hasControl(s) || !strings.Contains(s, shown) {
			t.Errorf("exec_poll %s = %q, want the output with its controls escaped", where, s)
		}
	}

	text, data, _ = toolCall(t, h, access, sid, "wanctl_peers", nil)
	listing, _ := json.Marshal(data)
	if hasControl(text) || hasControl(string(listing)) || !strings.Contains(text, `box\x1b]0;renamed\x07`) {
		t.Errorf("peers = %q / %s, want the device's name with its controls escaped", text, listing)
	}

	text, _, _ = toolCall(t, h, access, sid, "wanctl_exec", map[string]any{"target": target, "command": "true", "cwd": "/nonexistent-\x1b[2J-dir"})
	if hasControl(text) || !strings.Contains(text, `/nonexistent-\x1b[2J-dir`) {
		t.Errorf("a device's refusal = %q, want its controls escaped", text)
	}

	path := filepath.Join(t.TempDir(), "styled.txt")
	if err := os.WriteFile(path, []byte("\x1b[1mbold\x1b[0m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	text, data, isError := toolCall(t, h, access, sid, "wanctl_read", map[string]any{"target": target, "path": path})
	if isError || data["content"] != "\x1b[1mbold\x1b[0m\n" || !strings.Contains(text, "\x1b[1mbold") {
		t.Fatalf("read = %q / %v, want the file exactly as stored", text, data["content"])
	}
	if text, _, isError = toolCall(t, h, access, sid, "wanctl_edit", map[string]any{"target": target, "path": path, "old": "\x1b[1mbold", "new": "plain"}); isError {
		t.Fatalf("edit with the text read back: %s", text)
	}
	if b, _ := os.ReadFile(path); string(b) != "plain\x1b[0m\n" {
		t.Fatalf("file after edit = %q", b)
	}
}
