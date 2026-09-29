package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// liveHeap collects and returns the heap that survived: what the process
// holds, not garbage it has yet to reclaim.
func liveHeap() int64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return int64(ms.HeapAlloc)
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
	// The command produces all of its output, says so, and then keeps running
	// until the test lets it end, so that the server is still in the middle of
	// the call when the test looks at what it holds. It also ends if the test
	// goes away without letting it, taking the directory with it.
	//
	// yes, because it is fast everywhere: BSD tr, which macOS has, moves about
	// 30 MB/s, so on a Mac the device was the bottleneck and this test read
	// 1-2 MiB where Linux read up to 91.
	signals := t.TempDir()
	produced, finish := filepath.Join(signals, "produced"), filepath.Join(signals, "finish")
	t.Cleanup(func() { os.WriteFile(finish, nil, 0o600) })
	command := fmt.Sprintf("yes %s | head -c %d; printf 'the end\\n'; : > '%s'; "+
		"until [ -e '%s' ] || [ ! -d '%s' ]; do sleep 0.01; done",
		strings.Repeat("x", 63), outputBytes, produced, finish, signals)
	call, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{
		// One-shot: the output streams from the process in pipe-sized chunks,
		// the way a device that simply produces a lot of it sends it.
		"name": "wanctl_exec", "arguments": map[string]any{"target": target, "command": command, "oneshot": true},
	}})

	base := liveHeap()
	var (
		rr  *httptest.ResponseRecorder
		out map[string]any
	)
	replied := make(chan struct{})
	go func() {
		defer close(replied)
		rr, out = rpc(t, h, access, sid, string(call))
	}()
	for {
		if _, err := os.Stat(produced); err == nil {
			break
		}
		select {
		case <-replied:
			t.Fatalf("exec ended before its output was produced: %d %s", rr.Code, rr.Body.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
	// Every byte of the output is now somewhere in this process: in the
	// device's uploads, queued in the relay (which runs here too), in flight on
	// the server's connection to it, or with the server. Until the relay has
	// delivered it, the heap holds all of that, up to the relay's and the
	// connections' own budgets, plus garbage the collector has not caught up
	// with: sampling it then read up to 91 MiB on CI, and a profile of such a
	// peak puts 0.1 MiB of it with the server. What the server holds is what
	// is left once the relay has delivered everything: all of the output for a
	// server that kept it; for one that keeps only what it returns, that plus
	// what its connections keep between reads (the last chunk the relay
	// delivered, up to 16 MiB, and the device's upload buffer), 1.2-17 MiB
	// measured. Delivery takes as long as the machine takes and can pause for
	// a quarter of a second on the way, so collect until the heap is within
	// the bound and has not fallen for twice that, and give up only well past
	// it.
	const maxHeld = 32 << 20
	held, fell := liveHeap()-base, time.Now()
	for deadline := fell.Add(10 * time.Second); time.Now().Before(deadline); {
		if held <= maxHeld && time.Since(fell) >= 500*time.Millisecond {
			break
		}
		time.Sleep(10 * time.Millisecond)
		now := liveHeap() - base
		if now < held-1<<20 {
			fell = time.Now()
		}
		held = min(held, now)
	}
	os.WriteFile(finish, nil, 0o600)
	<-replied

	if rr.Code != http.StatusOK || out == nil {
		t.Fatalf("exec: %d %s", rr.Code, rr.Body.String())
	}
	result, _ := out["result"].(map[string]any)
	data, _ := result["structuredContent"].(map[string]any)
	if data == nil || data["code"] != float64(0) {
		t.Fatalf("exec result = %v", result)
	}
	stdout, _ := data["stdout"].(string)
	wantHead := fmt.Sprintf("[output truncated: showing last %d of %d bytes;", maxExecStream, outputBytes+len("the end\n"))
	if !strings.HasPrefix(stdout, wantHead) || !strings.HasSuffix(stdout, "xxxx\nthe end\n") {
		t.Errorf("stdout is not the marked tail: %.100q ... %q", stdout, stdout[max(0, len(stdout)-40):])
	}
	if _, tail, _ := strings.Cut(stdout, "\n"); len(tail) != maxExecStream {
		t.Errorf("returned %d bytes of output, want the %d-byte cap", len(tail), maxExecStream)
	}
	if data["stdout_truncated"] != true || data["stderr"] != "" || data["stderr_truncated"] != false {
		t.Errorf("truncation flags = %v, stderr = %q / %v", data["stdout_truncated"], data["stderr"], data["stderr_truncated"])
	}
	t.Logf("with all %d MiB of output produced and the command still running, the heap was %d KiB above where it started", outputBytes>>20, held>>10)
	if held > maxHeld {
		t.Fatalf("with all %d MiB of output produced and the command still running, the heap stayed %d MiB above where it started; the server should keep only what it returns", outputBytes>>20, held>>20)
	}
}
