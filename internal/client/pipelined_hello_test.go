package client

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wanctl/internal/agent"
	"wanctl/internal/relay"
)

// The request behind a pipelined hello is already on the wire when the device
// refuses the hello. It must not run, and the caller must still get the
// pairing link, not a generic read error.
func TestRefusedPipelinedHelloRunsNothingAndKeepsThePairingLink(t *testing.T) {
	for _, transportName := range []string{"http", "ws"} {
		t.Run(transportName, func(t *testing.T) {
			srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
			defer srv.Close()
			base := srv.URL
			if transportName == "ws" {
				base = "ws" + strings.TrimPrefix(srv.URL, "http")
			}
			t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
			t.Setenv("WANCTL_PORTAL", "https://portal.example.com")
			ag, err := agent.New(agent.Options{RelayURL: base, Token: "tok", Name: "home-pc", Transport: transportName})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(ag.Close)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go ag.Run(ctx)
			time.Sleep(300 * time.Millisecond)

			t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
			t.Setenv("WANCTL_RELAY", base)
			t.Setenv("WANCTL_TOKEN", "tok")
			t.Setenv("WANCTL_TRANSPORT", transportName)
			t.Setenv("WANCTL_LABEL", "test controller")
			c, err := New()
			if err != nil {
				t.Fatal(err)
			}
			trustServer(t, c, "home-pc")

			dir := t.TempDir()
			marker := filepath.Join(dir, "ran")
			pushed := filepath.Join(dir, "pushed.txt")
			local := filepath.Join(dir, "local.txt")
			os.WriteFile(local, []byte("payload"), 0o644)

			wantReject := func(op string, err error) {
				t.Helper()
				var rej *RejectError
				if !errors.As(err, &rej) || rej.PairingURL == "" {
					t.Fatalf("%s: error = %v, want a reject carrying the pairing link", op, err)
				}
			}
			_, err = c.ExecTo(context.Background(), ExecRequest{Target: "home-pc", Command: "touch " + marker, OneShot: true}, io.Discard, io.Discard)
			wantReject("exec", err)
			wantReject("push", c.Push(context.Background(), "home-pc", local, pushed))
			wantReject("pull", c.Pull(context.Background(), "home-pc", local, filepath.Join(dir, "back.txt")))
			wantReject("logs", c.LogsTo(context.Background(), "home-pc", "", "", "", 1, io.Discard))

			time.Sleep(200 * time.Millisecond)
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("the exec behind a refused hello ran on the device")
			}
			if _, err := os.Stat(pushed); err == nil {
				t.Fatal("the push behind a refused hello wrote the file")
			}
		})
	}
}
