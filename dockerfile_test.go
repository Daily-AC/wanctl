package main

import (
	"os"
	"strings"
	"testing"
)

func TestContainerRuntimeIsPinnedAndNonRoot(t *testing.T) {
	b, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(b)
	for _, want := range []string{
		"golang:1.26.6-alpine3.24@sha256:af8d6740070b8906d12eae1c3e3ea0957fb63f492051ea05e354c38ef9fe88df",
		"alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b",
		"adduser -S -D -H -u 10001",
		"mkdir -p /data /dist",
		"ENV WANCTL_CONFIG_DIR=/data",
		"ENV WANCTL_DIST_DIR=/dist",
		"EXPOSE 8080",
		"USER wanctl",
		"relay|portal",
		"mcp)",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("Dockerfile missing security/runtime contract %q", want)
		}
	}
	if strings.Contains(dockerfile, "alpine:3.20") {
		t.Fatal("Dockerfile still references EOL Alpine 3.20")
	}
}
