package agent

import (
	"runtime"
	"strings"
	"testing"
	"wanctl/internal/protocol"
)

func TestADBConsoleValidatesBeforePairing(t *testing.T) {
	a := &Agent{}
	for _, m := range []protocol.Message{
		{Kind: protocol.KindADBPair, PairPort: 0, PairCode: "123456"},
		{Kind: protocol.KindADBPair, PairPort: 12345, PairCode: "1;echo"},
	} {
		reply := a.handleConsoleRPC(m)
		if reply.Kind != protocol.KindError || strings.Contains(reply.Reason, m.PairCode) {
			t.Fatal("invalid pairing request accepted or echoed")
		}
	}
	if runtime.GOOS != "android" {
		reply := a.handleConsoleRPC(protocol.Message{Kind: protocol.KindADBPair, PairPort: 12345, PairCode: "123456"})
		if reply.Kind != protocol.KindError || !strings.Contains(reply.Reason, "Android") {
			t.Fatalf("non-Android response: %+v", reply)
		}
	}
}
