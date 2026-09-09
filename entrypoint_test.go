package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"wanctl/internal/config"
)

// Bare `wanctl` used to enroll this machine and start an agent on it, so anyone
// who ran it on a controller-only box to see what the command does turned that
// box into a controlled device. It prints the help and changes nothing now.
func TestBareInvocationPrintsHelpAndTouchesNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "wanctl")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cfg := filepath.Join(dir, "config")

	cmd := exec.Command(bin)
	// A relay is deliberately not configured: printing help must not reach the
	// first-run question, which would write one.
	cmd.Env = append(os.Environ(), "WANCTL_CONFIG_DIR="+cfg, "WANCTL_RELAY=", "WANCTL_TOKEN=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bare wanctl exited %v\n%s", err, out)
	}
	text := string(out)
	for _, want := range []string{"USAGE", "wanctl start", "wanctl login", "本机:"} {
		if !strings.Contains(text, want) {
			t.Fatalf("bare output is missing %q:\n%s", want, text)
		}
	}
	for _, name := range []string{"token", "agent.pid", "agent.lock", "cert.pem", "key.pem", "device_id"} {
		if _, err := os.Stat(filepath.Join(cfg, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("bare wanctl created %s (stat err %v)", name, err)
		}
	}
}

// An unknown subcommand still fails; printing help for no arguments must not
// turn typos into successes.
func TestUnknownSubcommandStillFails(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "wanctl")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "not-a-command")
	cmd.Env = append(os.Environ(), "WANCTL_CONFIG_DIR="+filepath.Join(dir, "config"))
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("unknown subcommand exited 0:\n%s", out)
	}
}

// `wanctl start` absorbed the enrollment that bare `wanctl` used to do, so a
// device with no token still onboards in one command. Nothing is started if the
// login fails.
func TestStartEnrollsWhenThereIsNoToken(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_TOKEN", "")
	t.Setenv("WANCTL_RELAY", "https://relay.example.test")
	t.Setenv("WANCTL_PORTAL", "https://portal.example.test")

	called := 0
	restore := enrollForStart
	enrollForStart = func(context.Context) (string, error) {
		called++
		return "", errors.New("portal unreachable")
	}
	defer func() { enrollForStart = restore }()

	err := cmdStart(context.Background())
	if err == nil || !strings.Contains(err.Error(), "portal unreachable") {
		t.Fatalf("start error = %v, want the enrollment failure", err)
	}
	if called != 1 {
		t.Fatalf("enrollment attempted %d times, want 1", called)
	}
	if pid := config.ReadPID(); pid != 0 {
		t.Fatalf("a failed login still recorded an agent (pid %d)", pid)
	}
}

// An agent that is already running is not re-enrolled and not restarted.
func TestStartWithAnAgentAlreadyRunningDoesNothing(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_TOKEN", "")
	lock, err := config.AcquireAgentLock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	called := 0
	restore := enrollForStart
	enrollForStart = func(context.Context) (string, error) { called++; return "tok", nil }
	defer func() { enrollForStart = restore }()

	if err := cmdStart(context.Background()); err != nil {
		t.Fatalf("start with a running agent = %v", err)
	}
	if called != 0 {
		t.Fatal("start re-ran the portal login for an agent that was already up")
	}
}

// `wanctl login` is the controller command: it takes a credential and never
// makes this machine a device.
func TestLoginNeverRecordsAnAgent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WANCTL_CONFIG_DIR", dir)
	t.Setenv("WANCTL_TOKEN", "already-provided")

	if err := cmdLogin(context.Background(), nil); err != nil {
		t.Fatalf("login = %v", err)
	}
	if pid := config.ReadPID(); pid != 0 {
		t.Fatalf("login recorded an agent pid (%d)", pid)
	}
	for _, name := range []string{"agent.pid", "agent.lock"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("login created %s (stat err %v)", name, err)
		}
	}
}

// The status line is what bare `wanctl` adds under the help, and it must read
// from the lock rather than start anything to find out.
func TestLocalStatusLineReportsBothHalves(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_TOKEN", "")
	if line := localStatusLine(); !strings.Contains(line, "未登录") || !strings.Contains(line, "agent 未运行") {
		t.Fatalf("fresh config dir = %q", line)
	}
	t.Setenv("WANCTL_TOKEN", "tok")
	lock, err := config.AcquireAgentLock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := config.WritePID(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	line := localStatusLine()
	if !strings.Contains(line, "已登录") || !strings.Contains(line, "agent 运行中") {
		t.Fatalf("logged in with a running agent = %q", line)
	}
}
