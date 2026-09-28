package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"wanctl/internal/agent"
	"wanctl/internal/mcpauth"
	"wanctl/internal/policy"
	"wanctl/internal/relay"
	"wanctl/internal/transport"
)

// hostedDevice starts a relay and a real device agent on it, and returns the
// hosted MCP handler with a bearer for the device's owner and the device's
// target, already paired and pinned.
func hostedDevice(t *testing.T) (h http.Handler, access, target string) {
	t.Helper()
	return hostedDeviceNamed(t, "chatty-device")
}

// hostedDeviceNamed is hostedDevice for a device that calls itself name.
func hostedDeviceNamed(t *testing.T, name string) (h http.Handler, access, target string) {
	t.Helper()
	previous := sessions
	t.Cleanup(func() { sessions = previous })
	relayServer := httptest.NewServer(relay.New(relay.EnvTokenStore("exec-test:alice")).Handler())
	t.Cleanup(relayServer.Close)
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir()) // where the device keeps its copy of long output
	ag, err := agent.New(agent.Options{RelayURL: relayServer.URL, Token: "exec-test", Name: name, AutoYes: true, Mode: policy.ModeBypass, Transport: "http"})
	if err != nil {
		t.Fatal(err)
	}
	deviceIdentity, err := transport.LoadOrCreateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); ag.Close() })
	go ag.Run(ctx)
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", relayServer.URL)
	t.Setenv("WANCTL_TRANSPORT", "http")
	h, err = HandlerWithOptions(Options{Seed: []byte(testSeed), EndpointPath: "/mcp", OAuth: &OAuthConfig{
		ResourceMetadataURL: "https://example.invalid/resource",
		Live:                func(ns, token string) bool { return ns == "alice" && token == "exec-test" },
	}})
	if err != nil {
		t.Fatal(err)
	}
	access, claim, err := mcpauth.SealAccess([]byte(testSeed), "alice", "exec-test", "exec", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c, hint := sessions.oauthSession(claim).client()
	if hint != nil {
		t.Fatal(toolText(hint))
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		peers, e := c.Peers(ctx)
		if e == nil && len(peers) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("registration: %v", e)
		}
		time.Sleep(10 * time.Millisecond)
	}
	target = "alice/" + ag.DeviceID()
	if _, err := c.PinServer(ctx, target, deviceIdentity.Fingerprint, false); err != nil {
		t.Fatal(err)
	}
	return h, access, target
}

// heapPeak samples the live heap until stop is called and reports the most it
// saw above the level when it started. HeapAlloc also counts garbage the
// collector has not reached: on a small CI runner 100 MiB of output arrives
// faster than a background collection keeps up, so already-dropped chunks read
// as growth. Each sample therefore collects first and reads what survived,
// which is what the test is about.
func heapPeak() (stop func() uint64) {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base, peak := ms.HeapAlloc, ms.HeapAlloc
	quit, done := make(chan struct{}), make(chan struct{})
	var once sync.Once
	go func() {
		defer close(done)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-quit:
				return
			case <-tick.C:
				runtime.GC()
				var now runtime.MemStats
				runtime.ReadMemStats(&now)
				peak = max(peak, now.HeapAlloc)
			}
		}
	}()
	return func() uint64 {
		once.Do(func() { close(quit) })
		<-done
		return peak - base
	}
}

// A device's output is the device's to choose: a command, or an agent that
// ignores the request to keep long output on its side, can send any amount.
// The hosted MCP server returns at most a fixed slice of it, and it must hold
// no more than that while the output arrives — not the whole of it until the
// command ends. What it returns is the same as before: the end of the output,
// under a line saying how much there was and where the rest is.
func TestHostedExecHoldsOnlyWhatItReturns(t *testing.T) {
	h, access, target := hostedDevice(t)
	sid := openSession(t, h, access)
	const outputBytes = 100 << 20
	command := fmt.Sprintf("head -c %d /dev/zero | tr '\\0' x; printf '\\nthe end\\n'", outputBytes)
	call, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{
		// One-shot: the output streams from the process in pipe-sized chunks,
		// the way a device that simply produces a lot of it sends it.
		"name": "wanctl_exec", "arguments": map[string]any{"target": target, "command": command, "oneshot": true},
	}})

	stop := heapPeak()
	rr, out := rpc(t, h, access, sid, string(call))
	peak := stop()

	if rr.Code != http.StatusOK || out == nil {
		t.Fatalf("exec: %d %s", rr.Code, rr.Body.String())
	}
	result, _ := out["result"].(map[string]any)
	data, _ := result["structuredContent"].(map[string]any)
	if data == nil || data["code"] != float64(0) {
		t.Fatalf("exec result = %v", result)
	}
	stdout, _ := data["stdout"].(string)
	wantHead := fmt.Sprintf("[output truncated: showing last %d of %d bytes;", maxExecStream, outputBytes+len("\nthe end\n"))
	if !strings.HasPrefix(stdout, wantHead) || !strings.HasSuffix(stdout, "xxxx\nthe end\n") {
		t.Errorf("stdout is not the marked tail: %.100q ... %q", stdout, stdout[max(0, len(stdout)-40):])
	}
	if _, tail, _ := strings.Cut(stdout, "\n"); len(tail) != maxExecStream {
		t.Errorf("returned %d bytes of output, want the %d-byte cap", len(tail), maxExecStream)
	}
	if data["stdout_truncated"] != true || data["stderr"] != "" || data["stderr_truncated"] != false {
		t.Errorf("truncation flags = %v, stderr = %q / %v", data["stdout_truncated"], data["stderr"], data["stderr_truncated"])
	}
	t.Logf("%d MiB of device output raised the heap by at most %d MiB", outputBytes>>20, peak>>20)
	// The relay runs in this process too and may legitimately queue up to its
	// 32 MiB per-direction budget when the reader is slower than the device
	// (a two-core CI runner is). Keeping the whole stream would read 100+.
	if peak > 64<<20 {
		t.Fatalf("receiving %d MiB of output raised the heap by %d MiB; it should keep only what it returns", outputBytes>>20, peak>>20)
	}
}
