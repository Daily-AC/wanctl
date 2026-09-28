package relay

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The agent control channel carries a registration and, after it, nothing a
// relay needs to read in full. Any one message on it is held to the size of a
// control request, so a connected agent cannot make the relay buffer a JSON
// value of any size.
func TestAgentControlChannelBoundsEachMessage(t *testing.T) {
	padding := strings.Repeat("A", 1<<20)
	agentOnline := func(r *Relay) bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.agents["alice/home-pc"] != nil
	}
	closedSoon := func(t *testing.T, read func() error, what string) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- read() }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("%s: the relay answered instead of closing", what)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: the relay kept the control channel open", what)
		}
	}

	t.Run("an oversized registration", func(t *testing.T) {
		r := New(EnvTokenStore("tok-alice:alice"))
		s := newPipeRelayServer(t, r.Handler())
		ctrl, _, err := s.wsDial(context.Background(), "/agent", "tok-alice")
		if err != nil {
			t.Fatal(err)
		}
		defer ctrl.Close()
		go json.NewEncoder(ctrl).Encode(map[string]string{"op": "register", "device": "home-pc", "padding": padding})
		closedSoon(t, func() error { _, err := ctrl.Read(make([]byte, 1)); return err }, "oversized registration")
		if agentOnline(r) {
			t.Fatal("an oversized registration was accepted")
		}
	})

	t.Run("an oversized message after registering", func(t *testing.T) {
		r := New(EnvTokenStore("tok-alice:alice"))
		s := newPipeRelayServer(t, r.Handler())
		ctrl := registerWSAgent(t, r, s, "tok-alice", "home-pc")
		defer ctrl.Close()
		go json.NewEncoder(ctrl).Encode(map[string]string{"op": "ping", "padding": padding})
		closedSoon(t, func() error { _, err := ctrl.Read(make([]byte, 1)); return err }, "oversized message")
		waitFor(t, func() bool { return !agentOnline(r) }, "the agent to be taken offline")
	})

	t.Run("ordinary messages", func(t *testing.T) {
		r := New(EnvTokenStore("tok-alice:alice"))
		s := newPipeRelayServer(t, r.Handler())
		ctrl := registerWSAgent(t, r, s, "tok-alice", "home-pc")
		defer ctrl.Close()
		enc := json.NewEncoder(ctrl)
		for range 100 { // many small messages add up past the bound, one by one
			if err := enc.Encode(map[string]string{"op": "ping", "padding": padding[:4<<10]}); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(100 * time.Millisecond)
		if !agentOnline(r) {
			t.Fatal("an agent sending ordinary messages was taken offline")
		}
	})
}
