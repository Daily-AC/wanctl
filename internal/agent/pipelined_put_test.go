package agent

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wanctl/internal/policy"
	"wanctl/internal/protocol"
	"wanctl/internal/relay"
	"wanctl/internal/transport"
)

// sendPipelinedPut writes a whole small upload without waiting for the
// device's acknowledgement, the way a controller now sends small files.
func sendPipelinedPut(t *testing.T, dr *transport.DialResult, path string, data []byte) {
	t.Helper()
	if err := protocol.WriteMessage(dr.Conn, protocol.Message{Kind: protocol.KindFilePut, Path: path, Size: int64(len(data)), Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteFrame(dr.Conn, protocol.FrameData, data); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteMessage(dr.Conn, protocol.Message{Kind: protocol.KindEOF}); err != nil {
		t.Fatal(err)
	}
}

// A refused upload whose bytes were already sent behind the request must
// leave nothing on disk: the gate decides before a single data frame is read.
func TestPipelinedPutIsRefusedBeforeAnyByteIsWritten(t *testing.T) {
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")
	startAgent(t, base, policy.DenyApprover{}, policy.ModeNormal)
	dr := connectController(t, base)
	defer dr.Conn.Close()

	path := filepath.Join(t.TempDir(), "guarded.txt")
	sendPipelinedPut(t, dr, path, []byte("should never land"))
	got, err := protocol.ReadMessage(dr.Conn)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != protocol.KindReject || !strings.Contains(got.Reason, "write denied by device policy") {
		t.Fatalf("reply = %s %q, want the policy refusal", got.Kind, got.Reason)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 0 {
		t.Fatalf("refused upload left %d entries in the directory", len(entries))
	}
}

func TestPipelinedPutLandsWhenAllowed(t *testing.T) {
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")
	startAgent(t, base, policy.DenyApprover{}, policy.ModeBypass)
	dr := connectController(t, base)
	defer dr.Conn.Close()

	path := filepath.Join(t.TempDir(), "small.txt")
	sendPipelinedPut(t, dr, path, []byte("hello"))
	for _, want := range []string{"ack", "done"} {
		got, err := protocol.ReadMessage(dr.Conn)
		if err != nil || got.Kind != protocol.KindOK {
			t.Fatalf("%s = %s %q err=%v", want, got.Kind, got.Reason, err)
		}
	}
	if b, _ := os.ReadFile(path); string(b) != "hello" {
		t.Fatalf("file holds %q", b)
	}
}
