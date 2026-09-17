package client

import (
	"errors"
	"io"
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

// A device that ran the operation and then lost the connection must not be
// reported as one that cannot run it at all. The two look identical on the wire
// — the session ends — and only one of them means "nothing happened".
func TestLostConnectionAfterSendIsNotAnUnsupportedAgent(t *testing.T) {
	for _, kind := range []string{protocol.KindFileEdit, protocol.KindFileWrite, protocol.KindFileRead} {
		t.Run(kind, func(t *testing.T) {
			device, controller := net.Pipe()
			defer controller.Close()
			go func() {
				// Read the request — the device has it, and in the edit and
				// write cases has already applied it — then drop the session
				// without answering.
				protocol.ReadMessage(device)
				device.Close()
			}()

			_, err := fileOpOver(controller, protocol.Message{
				Kind: kind, Path: "/etc/app.conf", Old: "a", New: "b", Content: "x\n",
			})
			var unsupported *UnsupportedError
			if errors.As(err, &unsupported) {
				t.Fatalf("a dropped connection was reported as an unsupported agent: %v", err)
			}
			var lost *ResultLostError
			if !errors.As(err, &lost) {
				t.Fatalf("error = %v, want a ResultLostError", err)
			}
			if lost.Kind != kind || lost.Path != "/etc/app.conf" {
				t.Errorf("lost result names %s %q", lost.Kind, lost.Path)
			}
			if !strings.Contains(err.Error(), "result unknown") {
				t.Errorf("message does not lead with the uncertainty: %q", err.Error())
			}
			if strings.Contains(err.Error(), "wanctl update") {
				t.Errorf("message tells the caller to update an agent that may be fine: %q", err.Error())
			}
			// A write or an edit may already be on disk; a read cannot be.
			if kind == protocol.KindFileRead {
				if !strings.Contains(err.Error(), "retry") {
					t.Errorf("a lost read should say retrying is safe: %q", err.Error())
				}
			} else if !strings.Contains(err.Error(), "compare sha256") {
				t.Errorf("a lost mutation should say to check the file: %q", err.Error())
			}
		})
	}
}

// The other half of the same distinction: an explicit refusal of the frame kind
// still means the device cannot do this, and still says to update it.
func TestExplicitUnknownRequestIsStillAnUnsupportedAgent(t *testing.T) {
	device, controller := net.Pipe()
	defer controller.Close()
	go func() {
		defer device.Close()
		if _, err := protocol.ReadMessage(device); err != nil {
			return
		}
		protocol.WriteMessage(device, protocol.Message{
			Kind: protocol.KindError, Reason: "unknown request: " + protocol.KindFileEdit,
		})
	}()

	_, err := fileOpOver(controller, protocol.Message{Kind: protocol.KindFileEdit, Path: "/etc/app.conf", Old: "a", New: "b"})
	var unsupported *UnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("error = %v, want an UnsupportedError", err)
	}
	var lost *ResultLostError
	if errors.As(err, &lost) {
		t.Error("an explicit refusal was read as a lost result")
	}
	if !strings.Contains(err.Error(), "wanctl update") {
		t.Errorf("message = %q, want the update instruction", err.Error())
	}
}

// deadAfterSend takes the request and then fails the read with whatever the
// transport would have said. net.Pipe can only end cleanly, and a clean end is
// the one case this is NOT about.
type deadAfterSend struct {
	sent bool
	err  error
}

func (d *deadAfterSend) Write(p []byte) (int, error) { d.sent = true; return len(p), nil }

func (d *deadAfterSend) Read([]byte) (int, error) {
	if !d.sent {
		return 0, errors.New("read before the request was sent")
	}
	return 0, d.err
}

// A connection that is reset or times out after the request was sent is exactly
// as unknown as one that ends cleanly, and must not come back as a bare
// transport error the caller reads as "it failed, so retry".
func TestAnyPostSendFailureIsALostResult(t *testing.T) {
	reset := &deadAfterSend{err: errors.New("connection reset by peer")}
	_, err := fileOpOver(reset, protocol.Message{
		Kind: protocol.KindFileWrite, Path: "/etc/app.conf", Content: "x\n",
	})
	var lost *ResultLostError
	if !errors.As(err, &lost) {
		t.Fatalf("error = %v (%T), want a ResultLostError", err, err)
	}
	if !strings.Contains(err.Error(), "result unknown") || !strings.Contains(err.Error(), "compare sha256") {
		t.Errorf("message = %q", err.Error())
	}
	// The cause is named, because a reset and a device that went away call for
	// different next steps even though both leave the result unknown.
	if !strings.Contains(err.Error(), "connection reset by peer") {
		t.Errorf("message drops the cause: %q", err.Error())
	}
	if lost.Cause == nil || !errors.Is(err, lost.Cause) {
		t.Errorf("cause = %v, want it kept and unwrappable", lost.Cause)
	}

	// A timeout is the same answer: nothing about the device's state is known.
	timeout := &deadAfterSend{err: os.ErrDeadlineExceeded}
	_, err = fileOpOver(timeout, protocol.Message{Kind: protocol.KindFileEdit, Path: "/etc/app.conf", Old: "a", New: "b"})
	if !errors.As(err, &lost) {
		t.Fatalf("a timed-out edit came back as %v (%T)", err, err)
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Error("the timeout is no longer detectable through errors.Is")
	}

	// A clean end says nothing extra: there is no cause worth naming.
	clean := &deadAfterSend{err: io.EOF}
	_, err = fileOpOver(clean, protocol.Message{Kind: protocol.KindFileEdit, Path: "/etc/app.conf", Old: "a", New: "b"})
	if !errors.As(err, &lost) {
		t.Fatalf("error = %v, want a ResultLostError", err)
	}
	if strings.Contains(err.Error(), "EOF") {
		t.Errorf("a clean end was reported with its plumbing showing: %q", err.Error())
	}
}
