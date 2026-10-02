package mcp

import (
	"context"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"wanctl/internal/agent"
	"wanctl/internal/policy"
	"wanctl/internal/relay"
	"wanctl/internal/transport"

	mcpapi "github.com/mark3labs/mcp-go/mcp"
)

// The surface a model actually uses has to be able to finish first contact, not
// only describe it: the refusal from a real tool call, the number and the code
// read out of it, the user's answer as the code their DEVICE prints, the pin,
// and then the original call going through. Anything mocked here would hide the
// one thing worth knowing — whether the handler's `number`/`code` path reaches a
// real device at all.
func TestMCPFirstContactEndsInAPinAndTheCommandRuns(t *testing.T) {
	t.Setenv("WANCTL_MCP_ALLOW_UNSAFE_TRUST_SERVER", "1")
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	defer srv.Close()

	// The device, with its own config dir so the test can compute the code the
	// device itself would print.
	deviceDir := t.TempDir()
	t.Setenv("WANCTL_CONFIG_DIR", deviceDir)
	ag, err := agent.New(agent.Options{
		RelayURL: srv.URL, Token: "tok", Name: "home-pc",
		AutoYes: true, Mode: policy.ModeBypass, Transport: "http",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	go ag.Run(ctx)
	time.Sleep(300 * time.Millisecond)

	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", srv.URL)
	t.Setenv("WANCTL_TOKEN", "tok")
	t.Setenv("WANCTL_TRANSPORT", "http")
	sessions = &sessionStore{stdio: &localFsSession{}}

	exec := func(command string) *mcpapi.CallToolResult {
		t.Helper()
		res, err := mcpExec(ctx, mcpapi.CallToolRequest{Params: mcpapi.CallToolParams{Arguments: map[string]any{
			"target": "home-pc", "command": command,
		}}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	// 1. First contact refuses, and the refusal carries what the user needs.
	first := exec("echo hi")
	if !first.IsError {
		t.Fatalf("first contact ran the command instead of refusing: %+v", first)
	}
	text := toolText(first)
	number := digitsFrom(t, text, `verification number:\s*([0-9 ]+)`, transport.NormalizeVerifyNumber)
	if !strings.Contains(text, "DEVICE IDENTITY CONFIRMATION REQUIRED") {
		t.Fatalf("refusal does not name the state:\n%s", text)
	}

	// 2. What the DEVICE answers for that number, computed here from the
	// device's own certificate — the value `wanctl verify` prints on it.
	onDevice := deviceCode(t, deviceDir, number)
	fpMatch := regexp.MustCompile(`fingerprint:\s+(\S+)`).FindStringSubmatch(text)
	if fpMatch == nil {
		t.Fatalf("refusal has no fingerprint: %s", text)
	}
	fingerprint := fpMatch[1]
	assertNoControllerCode(t, text, onDevice)

	// 3. A code nobody read off the device pins nothing.
	wrong := "000000000"
	if onDevice == wrong {
		wrong = "111111111"
	}
	denied, err := mcpTrustServer(ctx, mcpapi.CallToolRequest{Params: mcpapi.CallToolParams{Arguments: map[string]any{
		"target": "home-pc", "fingerprint": fingerprint, "number": number, "code": wrong,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if !denied.IsError || !strings.Contains(toolText(denied), "VERIFICATION CODE MISMATCH") {
		t.Fatalf("fabricated code accepted: %+v", denied)
	}
	assertNoControllerCode(t, toolText(denied), onDevice)
	if store := openTrustStore(t); len(store.List()) != 0 {
		t.Fatalf("a refused verification pinned something: %+v", store.List())
	}

	// 4. The code the device prints pins it.
	pinned, err := mcpTrustServer(ctx, mcpapi.CallToolRequest{Params: mcpapi.CallToolParams{Arguments: map[string]any{
		"target": "home-pc", "fingerprint": fingerprint, "number": number, "code": onDevice,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if pinned.IsError {
		t.Fatalf("pinning with the device's code failed: %s", toolText(pinned))
	}
	// The pin is keyed by the target the relay resolved, which is the durable
	// device ID, not the name the user typed.
	pinText := toolText(pinned)
	if !strings.Contains(pinText, "confirmed device identity") {
		t.Fatalf("pin did not report success: %s", pinText)
	}
	canonical := strings.Fields(strings.TrimSpace(strings.TrimPrefix(pinText, "confirmed device identity:")))[0]
	// Re-opened from disk, the way a later tool call would read it.
	store := openTrustStore(t)
	if got, ok := store.GetByName(canonical); !ok {
		t.Fatalf("nothing was pinned under %q: %+v", canonical, store.List())
	} else if want := deviceFingerprint(t, deviceDir); got.Fingerprint != want {
		t.Fatalf("pinned %s, the device's own certificate is %s", got.Fingerprint, want)
	}

	// 5. The original call now runs on the device.
	out := toolText(exec("echo hi"))
	if !strings.Contains(out, "hi") {
		t.Fatalf("exec after the pin returned %q", out)
	}
}

// openTrustStore re-opens the controller's on-disk trust store, the way a later
// tool call does. A store read once does not notice another process's write.
func openTrustStore(t *testing.T) *transport.Store {
	t.Helper()
	store, err := transport.OpenStore("known_servers.json")
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// deviceFingerprint reads the device's own certificate out of its config dir:
// the value both `wanctl verify` on that device and the controller's pin are
// derived from.
func deviceFingerprint(t *testing.T, deviceDir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(deviceDir, "cert.pem"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatalf("no PEM block in %s", filepath.Join(deviceDir, "cert.pem"))
	}
	return transport.Fingerprint(block.Bytes)
}

// deviceCode is the code the device's own `wanctl verify` would print.
func deviceCode(t *testing.T, deviceDir, number string) string {
	t.Helper()
	code, err := transport.VerifyCode(deviceFingerprint(t, deviceDir), number)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

// digitsFrom pulls a grouped number or code out of a message and normalizes it
// the way the commands take it.
func digitsFrom(t *testing.T, text, pattern string, normalize func(string) (string, error)) string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no match for %s in:\n%s", pattern, text)
	}
	digits, err := normalize(strings.ReplaceAll(strings.TrimSpace(m[1]), " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return digits
}
