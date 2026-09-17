package mcp

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"

	"wanctl/internal/client"
	"wanctl/internal/server"
)

// Output that fits is returned unchanged, and nothing on the device is written.
func TestShortOutputIsUntouchedAndLeavesNoFile(t *testing.T) {
	out := []byte("build ok\n")
	spill := server.NewSpill(&bytes.Buffer{}, maxExecStream)
	spill.Write(out)
	path, _, _ := spill.Close()

	if path != "" {
		t.Errorf("a short command spilled to %s", path)
	}
	if got := tailStream(out, client.ExecOutcome{SpillPath: path}); got != string(out) {
		t.Errorf("output = %q, want it unchanged", got)
	}
}

// The criterion this whole change exists for: a command that emits twice what
// can be returned gives back the LAST cap bytes, says so, and names a file on
// the device that really holds the whole thing.
func TestLongOutputReturnsTheTailAndNamesTheDeviceCopy(t *testing.T) {
	var whole bytes.Buffer
	for i := 0; whole.Len() < 2*maxExecStream; i++ {
		fmt.Fprintf(&whole, "line %06d: the quick brown fox jumps over the lazy dog\n", i)
	}
	// Exactly what the device does with a command's output.
	var toController bytes.Buffer
	spill := server.NewSpill(&toController, maxExecStream)
	spill.Write(whole.Bytes())
	path, total, kept := spill.Close()

	if path == "" {
		t.Fatal("the device kept no copy of an over-long output")
	}
	t.Cleanup(func() { os.Remove(path) })
	if total != int64(whole.Len()) {
		t.Errorf("device counted %d bytes, want %d", total, whole.Len())
	}

	got := tailStream(toController.Bytes(), client.ExecOutcome{SpillPath: path, SpillBytes: total, SpillKept: kept})
	head, tail, found := strings.Cut(got, "\n")
	if !found {
		t.Fatal("no truncation line")
	}
	wantHead := fmt.Sprintf("[output truncated: showing last %d of %d bytes;", maxExecStream, whole.Len())
	if !strings.HasPrefix(head, wantHead) {
		t.Errorf("truncation line = %q, want it to start %q", head, wantHead)
	}
	if !strings.Contains(head, path) {
		t.Errorf("truncation line does not name the device copy: %q", head)
	}
	if len(tail) != maxExecStream {
		t.Fatalf("returned %d bytes of output, want the %d-byte cap", len(tail), maxExecStream)
	}
	if tail != string(whole.Bytes()[whole.Len()-maxExecStream:]) {
		t.Error("what came back is not the END of the output")
	}
	// The end is where a command's result is, so the last line must survive.
	if !strings.HasSuffix(strings.TrimRight(tail, "\n"), "lazy dog") {
		t.Error("the last line was cut off")
	}

	onDevice, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the path in the message does not exist: %v", err)
	}
	if !bytes.Equal(onDevice, whole.Bytes()) {
		t.Fatalf("the device copy holds %d bytes, want the whole %d", len(onDevice), whole.Len())
	}
}

// A device too old to keep a copy must not be reported as if it had. The line
// says the output is gone and what to do so the next run is not.
func TestTruncationSaysWhenNothingWasKept(t *testing.T) {
	long := bytes.Repeat([]byte("x"), 2*maxExecStream)
	got := tailStream(long, client.ExecOutcome{})
	head, _, _ := strings.Cut(got, "\n")
	if !strings.Contains(head, "did not keep the full output") || !strings.Contains(head, "wanctl update") {
		t.Errorf("truncation line = %q", head)
	}
}

// Three silences, three different next moves. A caller that cannot tell them
// apart either hunts for a file that does not exist or updates an agent that is
// already current.
func TestTruncationSaysWhichSilenceThisIs(t *testing.T) {
	long := bytes.Repeat([]byte("x"), 2*maxExecStream)

	for _, tc := range []struct {
		name string
		res  client.ExecOutcome
		want []string
		deny []string
	}{
		{
			"an agent too old to have been asked",
			client.ExecOutcome{},
			[]string{"did not keep the full output", "wanctl update"},
			nil,
		},
		{
			"a device that counted but could not keep it",
			client.ExecOutcome{SpillBytes: int64(len(long))},
			[]string{"could not keep the full output", "narrow the command"},
			[]string{"wanctl update"},
		},
		{
			"a copy capped at the size limit",
			client.ExecOutcome{SpillPath: "/tmp/wanctl-exec-abc.log", SpillBytes: 20 << 20, SpillKept: 8 << 20},
			[]string{"kept the FIRST 8388608 bytes", "the middle is gone", "/tmp/wanctl-exec-abc.log"},
			nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head, _, _ := strings.Cut(tailStream(long, tc.res), "\n")
			for _, want := range tc.want {
				if !strings.Contains(head, want) {
					t.Errorf("line = %q, want it to contain %q", head, want)
				}
			}
			for _, deny := range tc.deny {
				if strings.Contains(head, deny) {
					t.Errorf("line = %q, which must not say %q", head, deny)
				}
			}
		})
	}
}
