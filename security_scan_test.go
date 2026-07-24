package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestImageScanForwardsProxyEnvironmentToTrivy(t *testing.T) {
	temp := t.TempDir()
	binDir := filepath.Join(temp, "bin")
	artifactsDir := filepath.Join(temp, "artifacts")
	logPath := filepath.Join(temp, "docker.log")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(artifactsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactsDir, "wanctl-image.tar"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	fakeDocker := `#!/bin/sh
printf 'ARGS %s\n' "$*" >>"$DOCKER_LOG"
printf 'ENV HTTP_PROXY=%s HTTPS_PROXY=%s http_proxy=%s https_proxy=%s\n' \
  "$HTTP_PROXY" "$HTTPS_PROXY" "$http_proxy" "$https_proxy" >>"$DOCKER_LOG"
`
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(fakeDocker), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", "scripts/security-scan.sh", "image")
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"DOCKER_LOG="+logPath,
		"WANCTL_ARTIFACTS_DIR="+artifactsDir,
		"HTTP_PROXY=http://127.0.0.1:7890",
		"HTTPS_PROXY=http://127.0.0.1:7890",
		"NO_PROXY=localhost",
		"http_proxy=http://127.0.0.1:7890",
		"https_proxy=http://127.0.0.1:7890",
		"no_proxy=localhost",
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("security scan failed: %v\n%s", err, output)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	args := string(log)
	if !strings.Contains(args, "--network host") {
		t.Errorf("docker run does not share the host network for a loopback proxy: %s", args)
	}
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"} {
		if !strings.Contains(args, "-e "+name) {
			t.Errorf("docker run does not inherit %s: %s", name, args)
		}
	}
	for _, line := range strings.Split(args, "\n") {
		if strings.HasPrefix(line, "ARGS ") && strings.Contains(line, "127.0.0.1:7890") {
			t.Fatalf("proxy values leaked into docker arguments: %s", args)
		}
	}
	for _, want := range []string{
		"HTTP_PROXY=http://127.0.0.1:7890",
		"HTTPS_PROXY=http://127.0.0.1:7890",
		"http_proxy=http://127.0.0.1:7890",
		"https_proxy=http://127.0.0.1:7890",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("container did not inherit the loopback proxy, want %q in %s", want, args)
		}
	}
}
