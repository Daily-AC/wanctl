package mcp

import "testing"

func TestRebindRoundTrip(t *testing.T) {
	ns, tok := "***REMOVED***", "tok_abc123.def-456_XYZ"
	got, err := func() (string, error) {
		s := encodeRebind(ns, tok)
		gns, gtok, err := decodeRebind(s)
		if err != nil {
			return "", err
		}
		if gns != ns || gtok != tok {
			t.Fatalf("round-trip mismatch: ns=%q tok=%q", gns, gtok)
		}
		return s, nil
	}()
	if err != nil {
		t.Fatal(err)
	}
	if got == "" {
		t.Fatal("empty credential")
	}
	if got == ns+"\x00"+tok {
		t.Fatal("credential must be encoded, not raw")
	}
}

func TestDecodeRebind_Malformed(t *testing.T) {
	if _, _, err := decodeRebind("!!!not base64!!!"); err == nil {
		t.Error("invalid base64 should error")
	}
	// Valid base64 but no NUL separator.
	if _, _, err := decodeRebind("aGVsbG8"); err == nil {
		t.Error("missing separator should error")
	}
}
