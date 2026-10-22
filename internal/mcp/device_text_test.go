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
// text, and whether it is an error.
func toolCall(t *testing.T, h http.Handler, access, sid, name string, args map[string]any) (string, bool) {
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
	isError, _ := result["isError"].(bool)
	return text.String(), isError
}

// hasControl reports a control character other than newline, carriage return
// and tab — something a terminal would act on rather than show.
func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		return (r < 0x20 && r != '\n' && r != '\r' && r != '\t') || (r >= 0x7f && r <= 0x9f)
	})
}

// What a device says reaches the model with its control characters made
// visible: command output, polled output and the reasons it gives. A file read
// through wanctl_read is the exception — it comes back exactly as stored, which
// is what lets wanctl_edit match it. (A device name carrying control characters
// no longer gets that far: the relay refuses it at registration.)
func TestDeviceControlCharactersReachTheModelEscaped(t *testing.T) {
	h, access, target := hostedDeviceNamed(t, "box")
	sid := openSession(t, h, access)
	const styled = `printf 'a\033[31mb\033[0m\tc\rd\001e\302\233f\n'`
	const shown = `a\x1b[31mb\x1b[0m` + "\tc\rd" + `\x01e\u009bf`

	text, _ := toolCall(t, h, access, sid, "wanctl_exec", map[string]any{"target": target, "command": styled, "oneshot": true})
	if hasControl(text) || !strings.Contains(text, shown) {
		t.Errorf("exec = %q, want the output with its controls escaped", text)
	}

	text, _ = toolCall(t, h, access, sid, "wanctl_exec_async", map[string]any{"target": target, "command": styled})
	job := unquoteMetadata(t, strings.TrimSpace(strings.TrimPrefix(text, "job_id: ")))
	for deadline := time.Now().Add(30 * time.Second); ; {
		text, _ = toolCall(t, h, access, sid, "wanctl_exec_poll", map[string]any{"target": target, "job_id": job})
		if strings.HasPrefix(text, "state: done") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background job did not finish")
		}
	}
	if hasControl(text) || !strings.Contains(text, shown) {
		t.Errorf("exec_poll = %q, want the output with its controls escaped", text)
	}

	text, _ = toolCall(t, h, access, sid, "wanctl_exec", map[string]any{"target": target, "command": "true", "cwd": "/nonexistent-\x1b[2J-dir"})
	if hasControl(text) || !strings.Contains(text, `/nonexistent-\x1b[2J-dir`) {
		t.Errorf("a device's refusal = %q, want its controls escaped", text)
	}

	path := filepath.Join(t.TempDir(), "styled.txt")
	if err := os.WriteFile(path, []byte("\x1b[1mbold\x1b[0m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	text, isError := toolCall(t, h, access, sid, "wanctl_read", map[string]any{"target": target, "path": path})
	if isError || !strings.HasSuffix(text, "--- content ---\n\x1b[1mbold\x1b[0m\n") {
		t.Fatalf("read = %q, want the file exactly as stored", text)
	}
	if text, isError = toolCall(t, h, access, sid, "wanctl_edit", map[string]any{"target": target, "path": path, "old": "\x1b[1mbold", "new": "plain"}); isError {
		t.Fatalf("edit with the text read back: %s", text)
	}
	if b, _ := os.ReadFile(path); string(b) != "plain\x1b[0m\n" {
		t.Fatalf("file after edit = %q", b)
	}
}
