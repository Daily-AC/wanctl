package client

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wanctl/internal/policy"
	"wanctl/internal/protocol"
)

// Write and a batch edit over the real relay, through a real agent: the file
// does not exist, one call creates it and its directory, a second call patches
// three places at once, and a read brings back exactly what is on disk.
func TestWriteAndMultiEditRoundTrip(t *testing.T) {
	c, ctx := startDevice(t, policy.ModeBypass)

	dir := t.TempDir()
	path := filepath.Join(dir, "conf", "app.toml")

	written, err := c.WriteFile(ctx, WriteRequest{
		Target:  "home-pc",
		Path:    path,
		Content: "alpha = 1\nbeta = 2\ngamma = 3\n",
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !written.Created {
		t.Error("a new file was not reported as created")
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "alpha = 1\nbeta = 2\ngamma = 3\n" {
		t.Fatalf("on disk: %q, %v", b, err)
	}

	edited, err := c.EditFile(ctx, EditRequest{
		Target:      "home-pc",
		Path:        path,
		ExpectedSHA: written.SHA256,
		Edits: []protocol.FileEdit{
			{Old: "alpha = 1", New: "alpha = 10"},
			{Old: "gamma = 3", New: "gamma = 30"},
		},
	})
	if err != nil {
		t.Fatalf("multi-edit: %v", err)
	}
	if edited.Replaced != 2 {
		t.Errorf("replaced = %d, want 2", edited.Replaced)
	}

	read, err := c.ReadFile(ctx, ReadRequest{Target: "home-pc", Path: path})
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if read.Content != "alpha = 10\nbeta = 2\ngamma = 30\n" {
		t.Fatalf("read back %q", read.Content)
	}
	if read.SHA256 != edited.SHA256 {
		t.Errorf("read and edit disagree about the hash: %s vs %s", read.SHA256, edited.SHA256)
	}
}

// A batch that cannot be applied leaves the device's file exactly as it was,
// across the wire as well as in the handler.
func TestRefusedBatchLeavesTheDeviceFileAlone(t *testing.T) {
	c, ctx := startDevice(t, policy.ModeBypass)

	dir := t.TempDir()
	path := filepath.Join(dir, "app.conf")
	const original = "x = 1\ny = 1\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := c.EditFile(ctx, EditRequest{
		Target: "home-pc", Path: path,
		Edits: []protocol.FileEdit{
			{Old: "x = 1", New: "x = 2"},
			{Old: "= 1", New: "= 9"},
		},
	})
	if err == nil {
		t.Fatal("an ambiguous entry was accepted")
	}
	if !strings.Contains(err.Error(), "edits[1]") {
		t.Errorf("refusal does not name the entry: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != original {
		t.Fatalf("file = %q after a refused batch", b)
	}
}

// The two forms are exclusive, and the controller says so without dialling.
func TestEditRefusesBothFormsLocally(t *testing.T) {
	c := &Client{}
	_, err := c.EditFile(t.Context(), EditRequest{
		Path: "/tmp/x", Old: "a", New: "b",
		Edits: []protocol.FileEdit{{Old: "a", New: "c"}},
	})
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("error = %v, want the exclusivity refusal", err)
	}
}

// A device whose agent predates write answers "unknown request", and the
// controller turns that into the one instruction that fixes it — naming write,
// not the frame kind, and not read/edit which that agent may well support.
func TestOldAgentUnknownWriteBecomesAnUpdateInstruction(t *testing.T) {
	device, controller := net.Pipe()
	defer controller.Close()
	go func() {
		defer device.Close()
		if _, err := protocol.ReadMessage(device); err != nil {
			return
		}
		protocol.WriteMessage(device, protocol.Message{
			Kind: protocol.KindError, Reason: "unknown request: " + protocol.KindFileWrite,
		})
	}()

	_, err := fileOpOver(controller, protocol.Message{
		Kind: protocol.KindFileWrite, Path: "/etc/app.conf", Content: "x\n",
	})
	var unsupported *UnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("error = %v, want an UnsupportedError", err)
	}
	if !strings.Contains(err.Error(), "does not support write") || !strings.Contains(err.Error(), "wanctl update") {
		t.Fatalf("message = %q, want the update instruction for write", err.Error())
	}
}
