package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpapi "github.com/mark3labs/mcp-go/mcp"
)

func callReq(args map[string]any) mcpapi.CallToolRequest {
	var req mcpapi.CallToolRequest
	req.Params.Arguments = args
	return req
}

func toolText(r *mcpapi.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if t, ok := c.(mcpapi.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// A prompt-injected model must not be able to read the operator's credential
// stores or wanctl's own config through wanctl_push / wanctl_pull
// (audit 2026-08-28, SEC-E-01).
func TestStdioLocalFileRefusesCredentialStores(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WANCTL_MCP_LOCAL_ROOT", "")
	cfg := filepath.Join(home, "wanctl-config")
	t.Setenv("WANCTL_CONFIG_DIR", cfg)
	os.MkdirAll(cfg, 0o700)
	project := filepath.Join(home, "project")
	os.MkdirAll(project, 0o700)
	t.Chdir(project)
	l := &localFsSession{}

	for _, bad := range []string{
		filepath.Join(home, ".ssh", "id_ed25519"),
		filepath.Join(home, ".config", "gh", "hosts.yml"),
		filepath.Join(home, ".aws", "credentials"),
		filepath.Join(cfg, "token"),
	} {
		if _, hint := l.localFile(bad, false); hint == nil {
			t.Errorf("%s was allowed", bad)
		}
	}
	for _, ok := range []string{
		filepath.Join(project, "app.apk"),
		filepath.Join(project, ".env"),
		filepath.Join(project, "subdir", "x"),
	} {
		if _, hint := l.localFile(ok, true); hint != nil {
			t.Errorf("%s was refused: %s", ok, toolText(hint))
		}
	}
	if _, hint := l.localFile(filepath.Join(home, "Downloads", "app.apk"), false); hint == nil {
		t.Fatal("a normal-looking path outside the MCP working directory was allowed")
	}
}

// With WANCTL_MCP_LOCAL_ROOT set, only that tree is reachable — including
// through a symlink that points out of it.
func TestStdioLocalFileHonoursRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	t.Setenv("WANCTL_MCP_LOCAL_ROOT", root)
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o600)
	os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "link"))
	l := &localFsSession{}

	if _, hint := l.localFile(filepath.Join(root, "a", "b.txt"), true); hint != nil {
		t.Fatalf("inside root refused: %s", toolText(hint))
	}
	if _, hint := l.localFile(filepath.Join(outside, "secret"), false); hint == nil {
		t.Fatal("outside root allowed")
	}
	if _, hint := l.localFile(filepath.Join(root, "..", filepath.Base(outside), "secret"), false); hint == nil {
		t.Fatal("dot-dot escape allowed")
	}
	if _, hint := l.localFile(filepath.Join(root, "link"), false); hint == nil {
		t.Fatal("symlink escape allowed")
	}
	// An explicit broad root never overrides the independent credential-store
	// deny rule.
	cfg := filepath.Join(root, "wanctl-config")
	t.Setenv("WANCTL_CONFIG_DIR", cfg)
	os.MkdirAll(cfg, 0o700)
	if _, hint := l.localFile(filepath.Join(cfg, "token"), false); hint == nil {
		t.Fatal("WANCTL_MCP_LOCAL_ROOT allowed wanctl's own token")
	}
}

// On a shared (HTTP) MCP server "local" is the server's own filesystem —
// /proc/self/environ holds the relay's admin secret — so the tools refuse
// before any login or dial happens.
func TestRemoteSessionRefusesLocalFiles(t *testing.T) {
	old := sessions
	sessions = &sessionStore{seed: []byte(strings.Repeat("s", 32)), m: map[string]*remoteSession{}}
	t.Cleanup(func() { sessions = old })
	for _, fn := range []func(context.Context, mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error){mcpPush, mcpPull} {
		res, err := fn(context.Background(), callReq(map[string]any{"target": "d", "local": "/proc/self/environ", "remote": "/tmp/x"}))
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError || !strings.Contains(toolText(res), "unavailable on a shared") {
			t.Fatalf("got %q", toolText(res))
		}
	}
	res, _ := mcpServerLogs(context.Background(), callReq(map[string]any{"service": "relay"}))
	if !res.IsError || !strings.Contains(toolText(res), "stdio mode only") {
		t.Fatalf("server_logs on a shared server: %q", toolText(res))
	}
}

// A browser page must not be able to drive a `wanctl mcp --http` server
// (DNS rebinding / localhost CSRF); programs send no Origin and pass.
func TestHandlerRefusesUnknownOrigins(t *testing.T) {
	t.Setenv("WANCTL_MCP_ALLOWED_ORIGINS", "https://host.example")
	old := sessions
	t.Cleanup(func() { sessions = old })
	h, err := Handler([]byte(strings.Repeat("k", 32)), "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	do := func(origin string) int {
		req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := do("https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("evil origin: %d", code)
	}
	if code := do("https://host.example"); code == http.StatusForbidden {
		t.Fatalf("allowed origin refused")
	}
	if code := do(""); code == http.StatusForbidden {
		t.Fatalf("no-origin (program) client refused")
	}
}
