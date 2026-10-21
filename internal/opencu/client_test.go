package opencu

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wanctl/internal/opencu/ocutest"
)

func TestOpenCURealMCPPeer(t *testing.T) {
	binary := ocutest.Build(t)
	log := filepath.Join(t.TempDir(), "calls")
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(), "OPENCU_TEST_LOG="+log)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Start(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	discovery, err := c.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		ServerInfo   struct{ Version string }
		Instructions string
		Tools        []map[string]json.RawMessage
	}
	if json.Unmarshal(discovery, &d) != nil || d.ServerInfo.Version != "0.3.99" || !strings.Contains(d.Instructions, "Observe") || len(d.Tools) != 2 {
		t.Fatalf("bad discovery: %s", discovery)
	}
	var pid int
	for i := 1; i <= 2; i++ {
		raw, err := c.Call(ctx, "peer_apply", json.RawMessage(`{"app":"Disposable","text":"private","error":true,"large":true}`))
		if err != nil {
			t.Fatal(err)
		}
		var r struct {
			Content []struct{ Type, Text, Data, MimeType string }
			IsError bool
			Meta    struct {
				PID, Calls int
				Exact      json.Number
				Echo       json.RawMessage
			} `json:"_meta"`
		}
		if json.Unmarshal(raw, &r) != nil || !r.IsError || len(r.Content) != 2 || r.Meta.Calls != i || r.Meta.Exact.String() != "9007199254740993" {
			t.Fatal("result fidelity lost")
		}
		if i == 1 {
			pid = r.Meta.PID
		} else if pid != r.Meta.PID {
			t.Fatal("connection was not reused")
		}
		png, err := base64.StdEncoding.DecodeString(r.Content[1].Data)
		if err != nil || len(png) < 3<<20 || string(png[:8]) != "\x89PNG\r\n\x1a\n" {
			t.Fatal("large PNG lost")
		}
	}
	_, err = c.Call(ctx, "peer_apply", json.RawMessage(`{"crash":true}`))
	if !errors.Is(err, ErrUnknown) {
		t.Fatalf("want unknown: %v", err)
	}
	c.Close()
	lines, _ := os.ReadFile(log)
	if len(strings.Split(strings.TrimSpace(string(lines)), "\n")) != 3 {
		t.Fatalf("call replayed: %s", lines)
	}
}
func TestOpenCUManifestAndStopCodes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir)
	if _, err := ReadManifest(); !errors.Is(err, ErrNotInstalled) || !strings.Contains(err.Error(), InstallCommand) {
		t.Fatal(err)
	}
	os.Mkdir(filepath.Join(dir, "OpenCU"), 0700)
	path := filepath.Join(dir, "OpenCU", "opencu.json")
	os.WriteFile(path, []byte(`{"mcp":{"command":"relative"}}`), 0600)
	if _, err := ReadManifest(); err == nil {
		t.Fatal("relative command accepted")
	}
	binary := filepath.Join(dir, "node.exe")
	arg := filepath.Join(dir, "server.mjs")
	raw, _ := json.Marshal(map[string]any{"mcp": map[string]any{"command": binary, "args": []string{arg}}})
	os.WriteFile(path, raw, 0600)
	m, err := ReadManifest()
	if err != nil || m.MCP.Command != binary || m.MCP.Args[0] != arg {
		t.Fatal(m, err)
	}
	for _, code := range []string{"USER_STOPPED", "STOPPED_BY_PERSON", "PERSON_ACTIVE"} {
		for _, raw := range []string{`{"content":[{"type":"image"},{"type":"text","text":"` + code + `: stop"}]}`, `{"_meta":{"opencu/error":"` + code + `"}}`} {
			if ErrorCode(json.RawMessage(raw)) != code {
				t.Fatal(raw)
			}
		}
	}
}

func TestInstalledVersionReadsManifestWithoutStartingEngine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir)
	if got := readInstalledVersion(filepath.Join(dir, "OpenCU", "opencu.json")); got != "" {
		t.Fatalf("missing install: %q", got)
	}
	if err := os.Mkdir(filepath.Join(dir, "OpenCU"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "OpenCU", "opencu.json")
	for _, version := range []string{"0.3.99", ""} {
		// No executable exists: discovery must only read metadata.
		raw, _ := json.Marshal(map[string]any{"version": version, "mcp": map[string]any{"command": filepath.Join(dir, "must-not-start")}})
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if got := readInstalledVersion(filepath.Join(dir, "OpenCU", "opencu.json")); got != version {
			t.Fatalf("got %q, want %q", got, version)
		}
	}
	// Invalid optional metadata must not make an otherwise usable act install fail.
	raw, _ := json.Marshal(map[string]any{"version": 123, "mcp": map[string]any{"command": filepath.Join(dir, "must-not-start")}})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManifest(); err != nil {
		t.Fatalf("metadata broke act discovery: %v", err)
	}
	if got := readInstalledVersion(filepath.Join(dir, "OpenCU", "opencu.json")); got != "" {
		t.Fatalf("non-string version: %q", got)
	}
	if err := os.WriteFile(path, []byte(`{"version":"1.0.0","mcp":{"command":"relative"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := readInstalledVersion(filepath.Join(dir, "OpenCU", "opencu.json")); got != "" {
		t.Fatalf("invalid install: %q", got)
	}
}
