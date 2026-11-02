package opencu

import (
	"context"
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

// startStopPeer starts a stop peer (see ocutest.BuildStopPeer), makes one call
// and returns the client and the file the peer logs its requests to.
func startStopPeer(t *testing.T, binary, when string) (*Client, string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "requests")
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(), "OPENCU_TEST_STOP="+when, "OPENCU_TEST_LOG="+log)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Start(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	if _, err := c.Call(ctx, "get_app_state", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	return c, log
}

func toolCalls(t *testing.T, log string) int {
	t.Helper()
	b, _ := os.ReadFile(log)
	return strings.Count(string(b), "tools/call")
}

func waitStopped(t *testing.T, c *Client) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !c.StoppedByPerson() {
		if time.Now().After(deadline) {
			t.Fatal("the client did not record a stop")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A stop the peer announces after a call is recorded without another call.
func TestClientRecordsAStopAnnouncedBetweenCalls(t *testing.T) {
	c, _ := startStopPeer(t, ocutest.BuildStopPeer(t), "after-call")
	waitStopped(t, c)
}

// Close reads what the peer's output still delivers after the peer exits: a
// stop announced then is recorded, and ordinary output then (a large log
// notification) is read to its end and not taken for a stop.
func TestClientCloseReadsOutputDeliveredAfterThePeerExits(t *testing.T) {
	binary := ocutest.BuildStopPeer(t)
	for _, tc := range []struct {
		when    string
		stopped bool
	}{{"on-exit", true}, {"late-stop-on-exit", true}, {"late-output-on-exit", false}} {
		t.Run(tc.when, func(t *testing.T) {
			c, _ := startStopPeer(t, binary, tc.when)
			if c.StoppedByPerson() {
				t.Fatal("stop recorded before it was announced")
			}
			c.Close()
			if c.StoppedByPerson() != tc.stopped {
				t.Fatalf("after close: stopped=%v, want %v", c.StoppedByPerson(), tc.stopped)
			}
		})
	}
}

// Output the client can no longer read (here a non-JSON line) could hide a
// later stop, so the peer counts as stopped, and no further request is sent.
func TestClientTreatsUnreadableOutputAsStoppedAndSendsNothing(t *testing.T) {
	c, log := startStopPeer(t, ocutest.BuildStopPeer(t), "garbage")
	waitStopped(t, c)
	if _, err := c.Call(context.Background(), "get_app_state", json.RawMessage(`{}`)); !errors.Is(err, ErrNotSent) {
		t.Fatalf("call after the output became unreadable: %v", err)
	}
	if n := toolCalls(t, log); n != 1 {
		t.Fatalf("peer received %d tool calls, want 1", n)
	}
}

// After the peer's output ended normally, a call is not sent and says so; a
// clean end is not a stop.
func TestClientDoesNotSendAfterThePeerExited(t *testing.T) {
	c, log := startStopPeer(t, ocutest.BuildStopPeer(t), "exit-after-call")
	select {
	case <-c.readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("peer output did not end")
	}
	if _, err := c.Call(context.Background(), "get_app_state", json.RawMessage(`{}`)); !errors.Is(err, ErrNotSent) {
		t.Fatalf("call after the peer exited: %v", err)
	}
	if n := toolCalls(t, log); n != 1 || c.StoppedByPerson() {
		t.Fatalf("tool calls=%d stopped=%v", n, c.StoppedByPerson())
	}
}

// Other log notifications are not a stop.
func TestClientIgnoresOtherNotifications(t *testing.T) {
	cmd := exec.Command(ocutest.Build(t))
	cmd.Env = append(os.Environ(), "OPENCU_TEST_LOG="+t.TempDir()+"/calls")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Start(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx, "peer_apply", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if c.StoppedByPerson() {
		t.Fatal("a diagnostic notification was taken for a stop")
	}
}
