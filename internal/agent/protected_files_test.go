package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"wanctl/internal/policy"
	"wanctl/internal/protocol"
)

// Every file channel a paired controller has — push, pull, and a workspace's
// relative file operations — refuses the device's own config dir however the
// path to it is written. Bypass mode is the case that matters: it is the one
// where policy grants the whole volume and nothing else stands in the way.
func TestFileChannelsRefuseConfigDirSpellings(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "wanctl")
	t.Setenv("WANCTL_CONFIG_DIR", cfg)
	a, err := New(Options{RelayURL: "http://unused", Token: "synthetic", Mode: policy.ModeBypass, Shell: "/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	for _, name := range []string{"token", "mode", "portal_admins.json"} {
		if err := os.WriteFile(filepath.Join(cfg, name), []byte("ORIGINAL\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	dirs := map[string]string{"canonical": cfg}
	if upper := filepath.Join(filepath.Dir(cfg), strings.ToUpper(filepath.Base(cfg))); upper != cfg {
		if info, err := os.Stat(upper); err == nil && info.IsDir() {
			dirs["case variant"] = upper
		}
	}
	if runtime.GOOS == "darwin" {
		if real, err := filepath.EvalSymlinks(cfg); err == nil {
			if _, err := os.Stat("/System/Volumes/Data" + real); err == nil {
				dirs["data-volume firmlink"] = "/System/Volumes/Data" + real
			}
		}
	}

	conn := pipeController(t, a, 1)
	for label, dir := range dirs {
		if r := roundTrip(t, conn, protocol.Message{Kind: protocol.KindFileGet, Path: filepath.Join(dir, "token")}); r.Kind != protocol.KindError {
			t.Errorf("%s: pull of the token answered %q", label, r.Kind)
			continue // a file_meta reply is followed by data this loop does not drain
		}
		if r := roundTrip(t, conn, protocol.Message{Kind: protocol.KindFilePut, Path: filepath.Join(dir, "portal_admins.json"), Size: 8, Mode: 0o600}); r.Kind != protocol.KindError {
			t.Fatalf("%s: push over portal_admins.json answered %q", label, r.Kind)
		}
	}

	// A workspace rooted at the directory holding the config dir: its relative
	// paths are the shortest spelling of all.
	ws := "w-" + strings.Repeat("c", 32)
	if r := roundTrip(t, conn, protocol.Message{Kind: protocol.KindWorkspace, Action: "open", WorkspaceID: ws, Path: filepath.Dir(cfg)}); r.Kind != protocol.KindWorkspace {
		t.Fatalf("workspace open: %+v", r)
	}
	for label, dir := range dirs {
		rel, err := filepath.Rel(filepath.Dir(cfg), dir)
		if err != nil || strings.HasPrefix(rel, "..") {
			rel = dir // the firmlink spelling is absolute
		}
		if r := roundTrip(t, conn, protocol.Message{Kind: protocol.KindWorkspace, Action: protocol.KindFileRead, WorkspaceID: ws, Path: filepath.Join(rel, "token")}); r.Kind != protocol.KindError {
			t.Errorf("%s: workspace read of the token answered %q: %+v", label, r.Kind, r.File)
		}
		if r := roundTrip(t, conn, protocol.Message{Kind: protocol.KindWorkspace, Action: protocol.KindFileWrite, WorkspaceID: ws, Path: filepath.Join(rel, "mode"), Content: "bypass\n"}); r.Kind != protocol.KindError {
			t.Errorf("%s: workspace write of mode answered %q", label, r.Kind)
		}
		if r := roundTrip(t, conn, protocol.Message{Kind: protocol.KindWorkspace, Action: protocol.KindFileEdit, WorkspaceID: ws, Path: filepath.Join(rel, "portal_admins.json"), Old: "ORIGINAL", New: "ATTACKER"}); r.Kind != protocol.KindError {
			t.Errorf("%s: workspace edit of portal_admins.json answered %q", label, r.Kind)
		}
	}
	for _, name := range []string{"token", "mode", "portal_admins.json"} {
		if got, err := os.ReadFile(filepath.Join(cfg, name)); err != nil || string(got) != "ORIGINAL\n" {
			t.Errorf("config %s changed: %q %v", name, got, err)
		}
	}
}
