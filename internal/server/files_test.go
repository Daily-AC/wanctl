package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenPolicyFileRejectsNonRegularFiles(t *testing.T) {
	root := t.TempDir()
	if f, err := openPolicyFile(root, root, false, 0); err == nil {
		f.Close()
		t.Fatal("directory was accepted as a downloadable regular file")
	}

	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(root, "missing"), link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if f, err := openPolicyFile(root, link, true, 0o600); err == nil {
		f.Close()
		t.Fatal("symbolic link was accepted as an upload target")
	}
}

func TestRootedNameRejectsLexicalEscape(t *testing.T) {
	root := t.TempDir()
	if _, _, err := rootedName(root, filepath.Join(root, "..", "outside")); err == nil {
		t.Fatal("path outside the policy root was accepted")
	}
}
