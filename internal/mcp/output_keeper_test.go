package mcp

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"

	"wanctl/internal/client"
)

// tailStream and clampStream render a whole stream the way the hosted server
// renders one that reached it in pieces. The tests written against the
// functions these names used to belong to (exectail_test.go, clamp_test.go)
// now exercise the keeper through them, unchanged.
func tailStream(b []byte, res client.ExecOutcome) string {
	k := keepTail()
	k.Write(b)
	return k.tailText(res)
}

func clampStream(b []byte) string {
	k := keepEnds()
	k.Write(b)
	return k.clampText()
}

// referenceTail and referenceClamp are the renderings exactly as they were
// when the server still buffered each stream whole: the oracle that says the
// keeper changed what is held, not what is shown.
func referenceTail(b []byte, res client.ExecOutcome) string {
	if len(b) <= maxExecStream {
		return string(b)
	}
	tail := b[len(b)-maxExecStream:]
	total := int64(len(b))
	if res.SpillBytes > total {
		total = res.SpillBytes
	}
	return fmt.Sprintf("[output truncated: showing last %d of %d bytes; %s]\n",
		len(tail), total, spillNote(res)) + string(tail)
}

func referenceClamp(b []byte) string {
	if len(b) <= maxExecStream {
		return string(b)
	}
	const headN = 8 * 1024
	tailN := maxExecStream - headN
	dropped := len(b) - headN - tailN
	var sb strings.Builder
	sb.Write(b[:headN])
	sb.WriteString(fmt.Sprintf(
		"\n\n[... wanctl truncated %d bytes (%d total); showing first %d + last %d. "+
			"Re-run with a tighter filter, e.g. `... | Select-Object -Last 200` or `... | tail -n 200` ...]\n\n",
		dropped, len(b), headN, tailN))
	sb.Write(b[len(b)-tailN:])
	return sb.String()
}

// Whatever the stream's length and however it is cut into writes, the keeper
// shows exactly what a buffer of the whole stream showed.
func TestKeptOutputReadsAsTheWholeStreamDid(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	outcomes := []client.ExecOutcome{
		{},
		{SpillPath: "/tmp/wanctl-exec-abc.log", SpillBytes: 9_000_000, SpillKept: 8 << 20},
		{SpillBytes: 1},
	}
	for _, size := range []int{0, 1, clampHead - 1, clampHead, clampHead + 1,
		maxExecStream - 1, maxExecStream, maxExecStream + 1, 2 * maxExecStream, 3*maxExecStream + 17, 1<<20 + 5} {
		data := make([]byte, size)
		for i := range data {
			data[i] = byte('a' + rng.IntN(26))
		}
		for _, chunk := range []int{1, 7, 4096, 32 << 10, 1 << 20} {
			tail, ends := keepTail(), keepEnds()
			for rest := data; len(rest) > 0; {
				n := min(chunk, len(rest))
				tail.Write(rest[:n])
				ends.Write(rest[:n])
				rest = rest[n:]
			}
			if got, want := ends.clampText(), referenceClamp(data); got != want {
				t.Fatalf("size %d in %d-byte writes: clamped output differs (%d vs %d bytes)", size, chunk, len(got), len(want))
			}
			for _, res := range outcomes {
				if got, want := tail.tailText(res), referenceTail(data, res); got != want {
					t.Fatalf("size %d in %d-byte writes: tail differs (%d vs %d bytes)", size, chunk, len(got), len(want))
				}
			}
		}
	}
}

// What a keeper holds, and what it allocates while 100 MiB go through it, does
// not depend on how much went through it.
func TestKeptOutputStaysBounded(t *testing.T) {
	frame := bytes.Repeat([]byte("x"), 1<<20)
	for name, k := range map[string]*outputKeeper{"stdout": keepTail(), "stderr": keepEnds()} {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		for range 100 {
			k.Write(frame)
		}
		runtime.ReadMemStats(&after)
		if k.total != 100<<20 {
			t.Fatalf("%s: counted %d bytes, want %d", name, k.total, 100<<20)
		}
		if held := cap(k.head) + cap(k.tail); held > 4*maxExecStream {
			t.Fatalf("%s: holds %d bytes of buffer for a %d-byte view", name, held, maxExecStream)
		}
		if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
			t.Fatalf("%s: allocated %d KiB while 100 MiB went through", name, allocated>>10)
		}
	}
}
