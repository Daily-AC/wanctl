package protocol

import (
	"bytes"
	"strings"
	"testing"
)

func TestDirectCapabilityAndOfferWire(t *testing.T) {
	var b bytes.Buffer
	if err := WriteMessage(&b, Message{Kind: KindFileGet, Direct: &DirectInfo{}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `"direct":{}`) {
		t.Fatalf("capability was not an empty object: %s", b.String())
	}
	m, err := ReadMessage(&b)
	if err != nil || m.Direct == nil || m.Kind != KindFileGet {
		t.Fatalf("round trip = %+v, %v", m, err)
	}
	if err := WriteMessage(&b, Message{Kind: KindDirectOffer, Direct: &DirectInfo{Candidates: []string{"192.0.2.1:123"}, CertSHA256: "pin"}}); err != nil {
		t.Fatal(err)
	}
	m, err = ReadMessage(&b)
	if err != nil || m.Direct == nil || m.Direct.CertSHA256 != "pin" || len(m.Direct.Candidates) != 1 {
		t.Fatalf("offer round trip = %+v, %v", m, err)
	}
}
