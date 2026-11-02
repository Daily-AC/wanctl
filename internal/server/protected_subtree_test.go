package server

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"wanctl/internal/protocol"
)

// A directory inside the config dir (the Android engine cache lives in one) is
// protected the same way as the config dir itself: by path, by the directory
// handle actually opened, and by the identity of the files inside it.
func TestConfigSubdirectoryIsProtectedLikeTheConfigDir(t *testing.T) {
	cfg := protectedConfig(t)
	sub := filepath.Join(cfg, "opencu")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(sub, "cache.json")
	if err := os.WriteFile(meta, []byte("ORIGINAL\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unchanged := func() {
		t.Helper()
		if b, err := os.ReadFile(meta); err != nil || string(b) != "ORIGINAL\n" {
			t.Fatalf("cache metadata changed: %q %v", b, err)
		}
	}

	for _, p := range []string{meta, filepath.Join(sub, "opencu-android-new.jar"), filepath.Join(sub, "nested", "x.json")} {
		if m := writeReply(t, "", protocol.Message{Kind: protocol.KindFileWrite, Path: p, Content: "{}"}, 1<<20); m.Kind != protocol.KindError {
			t.Errorf("file_write of %q answered %q", p, m.Kind)
		}
	}
	unchanged()

	// A directory handle that turns out to be the subdirectory, however it
	// was reached, is refused before anything is created in it.
	root, err := os.OpenRoot(sub)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := loadProtected().checkOpened(root, nil, "cache.json"); err == nil {
		t.Error("an opened handle on a config subdirectory was accepted")
	}

	link := filepath.Join(t.TempDir(), "innocent.json")
	if err := os.Link(meta, link); err != nil {
		t.Skipf("hard link unavailable: %v", err)
	}
	if f, err := openPolicyFile("", link); err == nil {
		f.Close()
		t.Error("a file inside a config subdirectory was readable through a hard link")
	}
	var buf bytes.Buffer
	handleFileRead(&buf, protocol.Message{Kind: protocol.KindFileRead, Path: link}, "")
	if m, err := protocol.ReadMessage(&buf); err != nil || m.Kind != protocol.KindError {
		t.Errorf("file_read through a hard link answered %q", m.Kind)
	}
	if m := writeReply(t, "", protocol.Message{Kind: protocol.KindFileWrite, Path: link, Content: "{}"}, 1<<20); m.Kind != protocol.KindError {
		t.Errorf("file_write through a hard link answered %q", m.Kind)
	}
	unchanged()
}

// A directory created inside the config dir after the protected set was
// loaded (the engine cache's directory on its first use), and a file created
// in it, are refused by the opened-handle check as well.
func TestConfigSubdirectoryCreatedAfterLoadIsRefused(t *testing.T) {
	cfg := protectedConfig(t)
	guard := loadProtected()
	late := filepath.Join(cfg, "opencu")
	if err := os.MkdirAll(late, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(late, "cache.json")
	if err := os.WriteFile(meta, []byte("ORIGINAL\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(late)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := guard.checkOpened(root, nil, "cache.json"); err == nil {
		t.Error("a config subdirectory created after load was accepted")
	}

	elsewhere := t.TempDir()
	link := filepath.Join(elsewhere, "innocent.json")
	if err := os.Link(meta, link); err != nil {
		t.Skipf("hard link unavailable: %v", err)
	}
	outside, err := os.OpenRoot(elsewhere)
	if err != nil {
		t.Fatal(err)
	}
	defer outside.Close()
	info, err := outside.Lstat("innocent.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := guard.checkOpened(outside, info, link); err == nil {
		t.Error("a file created in the config dir after load was accepted through a hard link")
	}
}
