package agent

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wanctl/internal/eventlog"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
	"wanctl/internal/relay"
	"wanctl/internal/transport"
)

func fileOpReply(t *testing.T, dr *transport.DialResult, m protocol.Message) protocol.Message {
	t.Helper()
	if err := protocol.WriteMessage(dr.Conn, m); err != nil {
		t.Fatal(err)
	}
	got, err := protocol.ReadMessage(dr.Conn)
	if err != nil {
		t.Fatalf("read reply to %s: %v", m.Kind, err)
	}
	return got
}

// file_read and file_edit are new verbs on an old question: may this controller
// see, or change, this file. They must reach the policy engine as the same
// KindRead/KindWrite that file_get and file_put already do, or a device owner's
// existing grants would mean something different depending on which verb the
// controller happened to use.
func TestFileReadAndEditUseTheSamePolicyKindsAsGetAndPut(t *testing.T) {
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	ap := requestKindApprover{kinds: make(chan policy.Kind, 4)}
	startAgent(t, base, ap, policy.ModeNormal)
	dr := connectController(t, base)
	defer dr.Conn.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "subject.txt")
	if err := os.WriteFile(path, []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// file_get, for the baseline: drain the download it starts.
	if err := protocol.WriteMessage(dr.Conn, protocol.Message{Kind: protocol.KindFileGet, Path: path}); err != nil {
		t.Fatal(err)
	}
	for {
		ft, payload, err := protocol.ReadFrame(dr.Conn)
		if err != nil {
			t.Fatal(err)
		}
		if ft != protocol.FrameJSON {
			continue
		}
		m, _ := protocol.DecodeMessage(payload)
		if m.Kind == protocol.KindEOF {
			break
		}
		if m.Kind == protocol.KindError || m.Kind == protocol.KindReject {
			t.Fatalf("file_get: %s %s", m.Kind, m.Reason)
		}
	}
	if got := fileOpReply(t, dr, protocol.Message{Kind: protocol.KindFileRead, Path: path}); got.Kind != protocol.KindFileResult {
		t.Fatalf("file_read: %s %s", got.Kind, got.Reason)
	}

	// file_put, for the baseline: acknowledge, stream, close.
	putPath := filepath.Join(dir, "written.txt")
	if got := fileOpReply(t, dr, protocol.Message{Kind: protocol.KindFilePut, Path: putPath, Size: 2, Mode: 0o644}); got.Kind != protocol.KindOK {
		t.Fatalf("file_put ack: %s %s", got.Kind, got.Reason)
	}
	if err := protocol.WriteFrame(dr.Conn, protocol.FrameData, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if got := fileOpReply(t, dr, protocol.Message{Kind: protocol.KindEOF}); got.Kind != protocol.KindOK {
		t.Fatalf("file_put commit: %s %s", got.Kind, got.Reason)
	}
	if got := fileOpReply(t, dr, protocol.Message{Kind: protocol.KindFileEdit, Path: path, Old: "world", New: "there"}); got.Kind != protocol.KindFileResult {
		t.Fatalf("file_edit: %s %s", got.Kind, got.Reason)
	}

	want := []policy.Kind{policy.KindRead, policy.KindRead, policy.KindWrite, policy.KindWrite}
	for i, wantKind := range want {
		select {
		case got := <-ap.kinds:
			if got != wantKind {
				t.Fatalf("request %d gated as %q, want %q", i, got, wantKind)
			}
		default:
			t.Fatalf("request %d never entered the policy gate", i)
		}
	}
}

// With no matching rule and a device that says no, both verbs must refuse in
// the same words as the whole-file operations they mirror — and leave the file
// alone. A refusal that read differently would look like a different kind of
// failure to whoever is reading the device's log.
func TestFileReadAndEditRefusalsMatchGetAndPut(t *testing.T) {
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	ag := startAgent(t, base, policy.DenyApprover{}, policy.ModeNormal)
	dr := connectController(t, base)
	defer dr.Conn.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "guarded.txt")
	const content = "secret\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	got := fileOpReply(t, dr, protocol.Message{Kind: protocol.KindFileRead, Path: path})
	if got.Kind != protocol.KindReject || !strings.Contains(got.Reason, "read denied by device policy") {
		t.Fatalf("file_read refusal = %q %q", got.Kind, got.Reason)
	}
	got = fileOpReply(t, dr, protocol.Message{Kind: protocol.KindFileEdit, Path: path, Old: "secret", New: "public"})
	if got.Kind != protocol.KindReject || !strings.Contains(got.Reason, "write denied by device policy") {
		t.Fatalf("file_edit refusal = %q %q", got.Kind, got.Reason)
	}
	if b, _ := os.ReadFile(path); string(b) != content {
		t.Fatalf("denied edit changed the file to %q", b)
	}

	// Both attempts are auditable as file events, with the path, exactly like a
	// refused push or pull.
	events, err := ag.log.Read(eventlog.Filter{Type: "file"})
	if err != nil {
		t.Fatal(err)
	}
	var sawRead, sawEdit bool
	for _, e := range events {
		if e.Detail == "READ "+path && e.Decision == "denied" {
			sawRead = true
		}
		if e.Detail == "EDIT "+path && e.Decision == "denied" {
			sawEdit = true
		}
	}
	if !sawRead || !sawEdit {
		t.Fatalf("file log missing a denied READ/EDIT entry: %+v", events)
	}
}

// Bypass is the switch a device owner flips to work unattended, and it covers
// file writes. An edit is a file write, so it must be covered too rather than
// stopping to ask on a device where nobody is watching.
func TestBypassModeCoversEdit(t *testing.T) {
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	startAgent(t, base, policy.DenyApprover{}, policy.ModeBypass)
	dr := connectController(t, base)
	defer dr.Conn.Close()

	path := filepath.Join(t.TempDir(), "auto.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := fileOpReply(t, dr, protocol.Message{Kind: protocol.KindFileEdit, Path: path, Old: "before", New: "after"})
	if got.Kind != protocol.KindFileResult {
		t.Fatalf("bypass-mode edit was refused: %s %s", got.Kind, got.Reason)
	}
	if b, _ := os.ReadFile(path); string(b) != "after\n" {
		t.Fatalf("file = %q", b)
	}
}
