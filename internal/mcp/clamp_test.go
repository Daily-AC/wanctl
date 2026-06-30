package mcp

import (
	"strings"
	"testing"
)

func TestClampStream_UnderLimitUnchanged(t *testing.T) {
	in := []byte("Default Distribution: Ubuntu-24.04\n")
	if got := clampStream(in); got != string(in) {
		t.Fatalf("small input must pass through unchanged; got %q", got)
	}
}

func TestClampStream_TruncatesExplicitlyWithHeadAndTail(t *testing.T) {
	head := strings.Repeat("H", 8*1024)
	tail := strings.Repeat("T", 40*1024)
	mid := strings.Repeat("M", 100*1024) // dropped
	in := []byte(head + mid + tail)

	got := clampStream(in)

	if len(got) >= len(in) {
		t.Fatalf("output should be shorter than input: got %d, in %d", len(got), len(in))
	}
	if !strings.Contains(got, "wanctl truncated") {
		t.Fatal("truncation must be explicit (#13): marker missing")
	}
	if !strings.HasPrefix(got, head) {
		t.Fatal("head must be preserved")
	}
	if !strings.HasSuffix(got, tail) {
		t.Fatal("tail must be preserved (callers want the tail)")
	}
	if strings.Contains(got, "MMMM") {
		t.Fatal("the dropped middle must not appear")
	}
}
