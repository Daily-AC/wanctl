//go:build !windows

package opencu

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestVersionReaderRejectsFIFOWithoutOpening(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencu.json")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() { done <- readInstalledVersion(path) }()
	select {
	case got := <-done:
		if got != "" {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO blocked metadata reader")
	}
}
