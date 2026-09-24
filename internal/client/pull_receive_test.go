package client

import (
	"bytes"
	"strings"
	"testing"

	"wanctl/internal/protocol"
)

func TestPullSizeMismatchNamesIncompleteFile(t *testing.T) {
	var wire bytes.Buffer
	if err := protocol.WriteFrame(&wire, protocol.FrameData, []byte("short")); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteMessage(&wire, protocol.Message{Kind: protocol.KindEOF}); err != nil {
		t.Fatal(err)
	}
	var dst bytes.Buffer
	got, err := receiveFile(&wire, &dst, 10, "/tmp/incomplete.bin")
	if got != 5 || err == nil || !strings.Contains(err.Error(), "incomplete") || !strings.Contains(err.Error(), "/tmp/incomplete.bin") {
		t.Fatalf("got %d, error %v", got, err)
	}
	if dst.String() != "short" {
		t.Fatalf("destination = %q", dst.String())
	}
}

func TestPullStreamFailureNamesIncompleteFile(t *testing.T) {
	var wire bytes.Buffer
	protocol.WriteFrame(&wire, protocol.FrameData, []byte("prefix"))
	protocol.WriteMessage(&wire, protocol.Message{Kind: protocol.KindError, Reason: "source disappeared"})
	got, err := receiveFile(&wire, &bytes.Buffer{}, 10, "partial.txt")
	if got != 6 || err == nil || !strings.Contains(err.Error(), "partial.txt") || !strings.Contains(err.Error(), "source disappeared") {
		t.Fatalf("got %d, error %v", got, err)
	}
}
