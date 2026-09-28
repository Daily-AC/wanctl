package server

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"wanctl/internal/protocol"
)

// protectedConfig makes a config dir holding the files an escalation would go
// after, and points the process at it.
func protectedConfig(t *testing.T) string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "wanctl")
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WANCTL_CONFIG_DIR", cfg)
	resetConfig(t, cfg)
	return cfg
}

// resetConfig puts every protected file back, so one spelling that got through
// does not make the next one look guilty too.
func resetConfig(t *testing.T, cfg string) {
	t.Helper()
	for _, name := range []string{"token", "key.pem", "portal_admins.json", "rules.json", "mode"} {
		if err := os.WriteFile(filepath.Join(cfg, name), []byte("ORIGINAL-"+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	os.RemoveAll(filepath.Join(cfg, "planted"))
}

// configSpellings returns other ways of writing cfg that the filesystem
// resolves to the same directory, each with a name for the failure message.
// Spellings a platform cannot produce are simply absent.
func configSpellings(t *testing.T, cfg string) map[string]string {
	t.Helper()
	out := map[string]string{
		"dot-dot": filepath.Join(cfg, "..", filepath.Base(cfg), ".", "logs", ".."),
	}
	// Case: APFS and NTFS are case-insensitive by default, so an upper-case
	// component names the same directory. Only meaningful where the volume
	// actually folds case.
	upper := filepath.Join(filepath.Dir(cfg), strings.ToUpper(filepath.Base(cfg)))
	if info, err := os.Stat(upper); err == nil && info.IsDir() {
		out["case variant"] = upper
	}
	// macOS firmlink: the data volume is reachable under a second path that is
	// not a symlink, so EvalSymlinks leaves it alone.
	if runtime.GOOS == "darwin" {
		if real, err := filepath.EvalSymlinks(cfg); err == nil {
			firm := "/System/Volumes/Data" + real
			if _, err := os.Stat(firm); err == nil {
				out["data-volume firmlink"] = firm
			}
		}
	}
	// A symlink to the config dir, and one to the directory above it.
	links := t.TempDir()
	if err := os.Symlink(cfg, filepath.Join(links, "to-config")); err == nil {
		out["symlink to the dir"] = filepath.Join(links, "to-config")
	}
	if err := os.Symlink(filepath.Dir(cfg), filepath.Join(links, "to-parent")); err == nil {
		out["symlink to its parent"] = filepath.Join(links, "to-parent", filepath.Base(cfg))
		if _, ok := out["case variant"]; ok {
			out["symlink then case variant"] = filepath.Join(links, "to-parent", strings.ToUpper(filepath.Base(cfg)))
		}
	}
	return out
}

// The device's identity key, namespace token, trust and policy files are never
// something a file operation may touch, however the path to them is written.
// The filesystem decides which spellings name the same file — case folding,
// the macOS data-volume firmlink, symlinks, dot-dot — so the refusal has to be
// decided the way the filesystem decides it, not by comparing strings.
func TestConfigFilesRefusedUnderEverySpelling(t *testing.T) {
	cfg := protectedConfig(t)
	spellings := configSpellings(t, cfg)
	if testing.Verbose() {
		t.Logf("spellings exercised: %d", len(spellings))
	}
	unchanged := func(t *testing.T, name string) {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(cfg, name))
		if err != nil || string(got) != "ORIGINAL-"+name+"\n" {
			t.Errorf("config %s was modified: %q %v", name, got, err)
		}
	}
	for label, dir := range spellings {
		t.Run(label, func(t *testing.T) {
			resetConfig(t, cfg)
			for _, name := range []string{"token", "key.pem"} {
				if f, err := openPolicyFile("", filepath.Join(dir, name)); err == nil {
					b := make([]byte, 64)
					n, _ := f.Read(b)
					f.Close()
					t.Errorf("read %s through %q -> %q", name, dir, b[:n])
				}
				var buf bytes.Buffer
				handleFileRead(&buf, protocol.Message{Kind: protocol.KindFileRead, Path: filepath.Join(dir, name)}, "")
				if m, err := protocol.ReadMessage(&buf); err != nil || m.Kind != protocol.KindError {
					t.Errorf("file_read of %s through %q answered %q", name, dir, m.Kind)
				}
			}

			if up, err := newPendingUpload("", filepath.Join(dir, "portal_admins.json"), 0o600); err == nil {
				up.file.Write([]byte("ATTACKER\n"))
				up.commit()
				up.abort()
				t.Errorf("upload over portal_admins.json through %q was accepted", dir)
			}
			unchanged(t, "portal_admins.json")

			// A write rule on the directory above the config dir is the grant
			// that makes the policy root reach it.
			parentRoot := filepath.Dir(cfg)
			if m := writeReply(t, parentRoot, protocol.Message{Kind: protocol.KindFileWrite, Path: filepath.Join(dir, "mode"), Content: "bypass\n"}, 1<<20); m.Kind != protocol.KindError {
				t.Errorf("file_write of mode through %q answered %q", dir, m.Kind)
			}
			unchanged(t, "mode")

			var buf bytes.Buffer
			handleFileEdit(&buf, protocol.Message{Kind: protocol.KindFileEdit, Path: filepath.Join(dir, "rules.json"), Old: "ORIGINAL", New: "EDITED"}, "", protocol.MaxEditBytes)
			if m, err := protocol.ReadMessage(&buf); err != nil || m.Kind != protocol.KindError {
				t.Errorf("file_edit of rules.json through %q answered %q", dir, m.Kind)
			}
			unchanged(t, "rules.json")

			// Nothing new may appear inside it either, including a directory
			// that file_write would create on the way.
			if m := writeReply(t, "", protocol.Message{Kind: protocol.KindFileWrite, Path: filepath.Join(dir, "planted", "x.json"), Content: "{}"}, 1<<20); m.Kind != protocol.KindError {
				t.Errorf("file_write of a new file through %q answered %q", dir, m.Kind)
			}
			if _, err := os.Stat(filepath.Join(cfg, "planted")); err == nil {
				t.Errorf("file_write through %q created a directory inside the config dir", dir)
			}
		})
	}
}

// A second name for the same file is still that file. A hard link outside the
// config dir has no protected directory anywhere on its path, so this is
// decided by what was opened, not by where the path points.
func TestConfigFileRefusedThroughAHardLink(t *testing.T) {
	cfg := protectedConfig(t)
	link := filepath.Join(t.TempDir(), "innocent.txt")
	if err := os.Link(filepath.Join(cfg, "token"), link); err != nil {
		t.Skipf("hard link unavailable: %v", err)
	}
	if f, err := openPolicyFile("", link); err == nil {
		b := make([]byte, 64)
		n, _ := f.Read(b)
		f.Close()
		t.Fatalf("the token was readable through a hard link: %q", b[:n])
	}
	var buf bytes.Buffer
	handleFileRead(&buf, protocol.Message{Kind: protocol.KindFileRead, Path: link}, "")
	if m, err := protocol.ReadMessage(&buf); err != nil || m.Kind != protocol.KindError {
		t.Fatalf("file_read through a hard link answered %q", m.Kind)
	}
}

// The running binary is refused under a case-variant spelling too: replacing
// it is replacing what the device runs next.
func TestRunningBinaryRefusedUnderCaseVariant(t *testing.T) {
	protectedConfig(t)
	self, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	variant := filepath.Join(filepath.Dir(self), strings.ToUpper(filepath.Base(self)))
	if variant == self {
		t.Skip("binary name has no case to vary")
	}
	if _, err := os.Stat(variant); err != nil {
		t.Skip("volume is case-sensitive")
	}
	if f, err := openPolicyFile("", variant); err == nil {
		f.Close()
		t.Error("the running binary was readable through a case variant")
	}
	if up, err := newPendingUpload("", variant, 0o755); err == nil {
		up.abort() // never commit: that would replace the test binary
		t.Error("an upload over the running binary was accepted through a case variant")
	}
}

// An ordinary file next to the config dir, and one whose name merely starts
// with the config dir's name, stay transferable: the refusal is about what the
// file is, not what its path looks like.
func TestNeighboursOfTheConfigDirStayTransferable(t *testing.T) {
	cfg := protectedConfig(t)
	sibling := cfg + "-notes"
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(filepath.Dir(cfg), "data.txt"), filepath.Join(sibling, "data.txt")} {
		if err := os.WriteFile(p, []byte("ok\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		f, err := openPolicyFile("", p)
		if err != nil {
			t.Errorf("an ordinary file %q was refused: %v", p, err)
			continue
		}
		f.Close()
		if m := writeReply(t, "", protocol.Message{Kind: protocol.KindFileWrite, Path: p, Content: "changed\n"}, 1<<20); m.Kind != protocol.KindFileResult {
			t.Errorf("writing an ordinary file %q answered %q %s", p, m.Kind, m.Reason)
		}
	}
}

// Where the filesystem cannot be asked, the string comparison that stands in
// for it must fold case exactly where the platform's default volumes do.
func TestPathComparisonFoldsCaseWhereTheVolumeDoes(t *testing.T) {
	cases := []struct {
		goos, target, dir string
		want              bool
	}{
		{"windows", `C:\Users\A\AppData\Roaming\WANCTL\token`, `C:\Users\a\AppData\Roaming\wanctl`, true},
		{"windows", `c:\users\a\appdata\roaming\wanctl`, `C:\Users\A\AppData\Roaming\wanctl`, true},
		{"windows", `C:\Users\a\AppData\Roaming\wanctl-notes\x`, `C:\Users\a\AppData\Roaming\wanctl`, false},
		{"darwin", "/Users/a/Library/Application Support/WANCTL/key.pem", "/Users/a/Library/Application Support/wanctl", true},
		{"linux", "/home/a/.config/WANCTL/key.pem", "/home/a/.config/wanctl", false},
		{"linux", "/home/a/.config/wanctl/key.pem", "/home/a/.config/wanctl", true},
	}
	for _, c := range cases {
		if got := pathWithinOn(c.goos, c.target, c.dir); got != c.want {
			t.Errorf("pathWithinOn(%s, %q, %q) = %v, want %v", c.goos, c.target, c.dir, got, c.want)
		}
	}
}
