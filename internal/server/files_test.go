package server

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"wanctl/internal/protocol"
)

type brokenFileReader struct{ sent bool }

func (r *brokenFileReader) Read(b []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(b, "prefix"), nil
	}
	return 0, errors.New("source disappeared")
}

func TestFileGetReadFailureSendsErrorFrame(t *testing.T) {
	var wire bytes.Buffer
	streamFileGet(&wire, &brokenFileReader{})
	ft, payload, err := protocol.ReadFrame(&wire)
	if err != nil || ft != protocol.FrameData || string(payload) != "prefix" {
		t.Fatalf("first frame = %d %q %v", ft, payload, err)
	}
	m, err := protocol.ReadMessage(&wire)
	if err != nil || m.Kind != protocol.KindError || m.Reason != "source disappeared" {
		t.Fatalf("failure frame = %+v %v", m, err)
	}
	if _, err := protocol.ReadMessage(&wire); err != io.EOF {
		t.Fatalf("extra frame: %v", err)
	}
}

func TestOpenPolicyFileRejectsNonRegularFiles(t *testing.T) {
	root := t.TempDir()
	if f, err := openPolicyFile(root, root); err == nil {
		f.Close()
		t.Fatal("directory was accepted as a downloadable regular file")
	}

	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(root, "missing"), link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if upload, err := newPendingUpload(root, link, 0o600); err == nil {
		upload.abort()
		t.Fatal("symbolic link was accepted as an upload target")
	}
}

func TestRootedNameRejectsLexicalEscape(t *testing.T) {
	root := t.TempDir()
	if _, _, err := rootedName(root, filepath.Join(root, "..", "outside")); err == nil {
		t.Fatal("path outside the policy root was accepted")
	}
}
