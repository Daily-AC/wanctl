package protocol

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestConsoleMessageRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	state := json.RawMessage(`{"mode":"normal","pending":[]}`)
	in := Message{Kind: KindModeSet, ConsoleMode: "normal", Data: state}
	if err := WriteMessage(&buf, in); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := ReadMessage(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out.Kind != KindModeSet || out.ConsoleMode != "normal" {
		t.Fatalf("bad header: %+v", out)
	}
	if string(out.Data) != string(state) {
		t.Fatalf("data lost: %s", out.Data)
	}
}

func TestDecodeDecide(t *testing.T) {
	m := Message{Kind: KindDecide, ApprovalID: "abc", Verdict: "y", Approver: "portal:me@corp"}
	b, _ := json.Marshal(m)
	got, err := DecodeMessage(b)
	if err != nil || got.ApprovalID != "abc" || got.Verdict != "y" || got.Approver != "portal:me@corp" {
		t.Fatalf("decode decide: %+v err=%v", got, err)
	}
}
