package client

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"wanctl/internal/agent"
	"wanctl/internal/policy"
	"wanctl/internal/relay"
)

// A cancelled context must reach the device. The CLI turns Ctrl-C into exactly
// this cancellation and then exits 130, so if Exec swallowed it the command
// would go on running on the device with nobody reading it (#37).
func TestExecCancelledContextKillsTheRemoteCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("observes the remote process with POSIX signals and /bin/sh job control")
	}
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	ag, err := agent.New(agent.Options{RelayURL: base, Token: "tok", Name: "home-pc", AutoYes: true, Mode: policy.ModeBypass})
	if err != nil {
		t.Fatal(err)
	}
	agentCtx, stopAgent := context.WithCancel(context.Background())
	defer stopAgent()
	go ag.Run(agentCtx)
	time.Sleep(200 * time.Millisecond)

	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", base)
	t.Setenv("WANCTL_TOKEN", "tok")
	t.Setenv("WANCTL_TRANSPORT", "ws")
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	trustServer(t, c, "home-pc")

	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
			if b, err := os.ReadFile(pidFile); err == nil && strings.TrimSpace(string(b)) != "" {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond)
		cancel() // what SIGINT does to the CLI's context
	}()

	var stdout, stderr bytes.Buffer
	code, err := c.ExecTo(ctx, ExecRequest{
		Target: "home-pc", Command: "sleep 60 & echo $! > " + pidFile + "; wait", OneShot: true,
	}, &stdout, &stderr)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled exec: code=%d err=%v", code, err)
	}
	if code == 0 {
		t.Fatal("cancelled exec reported success")
	}

	b, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("remote command never started: %v", readErr)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	defer syscall.Kill(pid, syscall.SIGKILL)
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("remote process %d survived the cancelled exec", pid)
}
