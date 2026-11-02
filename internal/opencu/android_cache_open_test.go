//go:build darwin || linux

package opencu

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// cacheFileChangedAfterLstat writes a 4-byte regular file and arranges for
// change to run between openRegular's Lstat and its open.
func cacheFileChangedAfterLstat(t *testing.T, change func(path string)) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cache.json")
	if err := os.WriteFile(path, []byte("good"), 0o600); err != nil {
		t.Fatal(err)
	}
	afterCacheLstat = change
	t.Cleanup(func() { afterCacheLstat = nil })
	return path
}

func TestCacheOpenAcceptsAnUnchangedRegularFile(t *testing.T) {
	path := cacheFileChangedAfterLstat(t, func(string) {})
	f, err := openRegular(path, 4)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// A FIFO that replaces the file after the check is refused at once, not
// waited on.
func TestCacheOpenDoesNotBlockOnAFIFOSwappedIn(t *testing.T) {
	path := cacheFileChangedAfterLstat(t, func(path string) {
		os.Remove(path)
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Error(err)
		}
	})
	done := make(chan error, 1)
	go func() {
		f, err := openRegular(path, 4)
		if err == nil {
			f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO was opened as a cache file")
		}
	case <-time.After(5 * time.Second):
		// Supply a writer so the blocked open returns and nothing is left running.
		if w, err := os.OpenFile(path, os.O_RDWR, 0); err == nil {
			w.Close()
		}
		<-done
		t.Fatal("opening a FIFO swapped in after the check blocked")
	}
}

// A symlink that replaces the file after the check is not followed, even when
// it points at the very file the check saw.
func TestCacheOpenDoesNotFollowASymlinkSwappedIn(t *testing.T) {
	path := cacheFileChangedAfterLstat(t, func(path string) {
		moved := path + ".moved"
		if err := os.Rename(path, moved); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(moved, path); err != nil {
			t.Error(err)
		}
	})
	if f, err := openRegular(path, 4); err == nil {
		f.Close()
		t.Fatal("a symlink was followed")
	}
}

// A file that grows past the limit after the check is refused.
func TestCacheOpenRefusesAFileThatGrewPastItsLimit(t *testing.T) {
	path := cacheFileChangedAfterLstat(t, func(path string) {
		if err := os.WriteFile(path, []byte("good and then some"), 0o600); err != nil {
			t.Error(err)
		}
	})
	if f, err := openRegular(path, 4); err == nil {
		f.Close()
		t.Fatal("a file over its limit was opened")
	}
}
