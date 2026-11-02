package opencu

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"

	"wanctl/internal/opencu/ocutest"
)

func startStopPeer(t *testing.T, binary, when string) *Client {
	t.Helper()
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(), "OPENCU_TEST_STOP="+when)
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
	return c
}

// A stop the peer announces after a call is recorded without another call.
func TestClientRecordsAStopAnnouncedBetweenCalls(t *testing.T) {
	c := startStopPeer(t, ocutest.BuildStopPeer(t), "after-call")
	deadline := time.Now().Add(5 * time.Second)
	for !c.StoppedByPerson() {
		if time.Now().After(deadline) {
			t.Fatal("stop announced while idle was not recorded")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A stop the peer announces as it exits is read before the client closes.
func TestClientRecordsAStopAnnouncedAsThePeerExits(t *testing.T) {
	c := startStopPeer(t, ocutest.BuildStopPeer(t), "on-exit")
	if c.StoppedByPerson() {
		t.Fatal("stop recorded before it was announced")
	}
	c.Close()
	if !c.StoppedByPerson() {
		t.Fatal("stop announced on exit was lost when the client closed")
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
