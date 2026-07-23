package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"wanctl/internal/protocol"
)

func TestInterruptedUploadPreservesExistingFile(t *testing.T) {
	allowed := t.TempDir()
	target := filepath.Join(allowed, "target.txt")
	if err := os.WriteFile(target, []byte("original"), 0o640); err != nil {
		t.Fatal(err)
	}
	conn := filePolicyConn(t, allowed)
	if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindFilePut, Path: target, Size: 12, Mode: 0o600}); err != nil {
		t.Fatal(err)
	}
	if reply, err := protocol.ReadMessage(conn); err != nil || reply.Kind != protocol.KindOK {
		t.Fatalf("put ack: reply=%+v err=%v", reply, err)
	}
	if err := protocol.WriteFrame(conn, protocol.FrameData, []byte("partial")); err != nil {
		t.Fatal(err)
	}
	conn.Close()

	deadline := time.Now().Add(time.Second)
	for {
		got, err := os.ReadFile(target)
		if err == nil && string(got) == "original" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("interrupted upload changed target to %q (err=%v)", got, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	assertNoUploadTemps(t, allowed)
}

func TestUploadRejectsSizeMismatchAndInvalidEOF(t *testing.T) {
	tests := []struct {
		name string
		eof  protocol.Message
	}{
		{name: "short payload", eof: protocol.Message{Kind: protocol.KindEOF}},
		{name: "wrong control frame", eof: protocol.Message{Kind: protocol.KindExec}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowed := t.TempDir()
			target := filepath.Join(allowed, "target.txt")
			if err := os.WriteFile(target, []byte("original"), 0o640); err != nil {
				t.Fatal(err)
			}
			conn := filePolicyConn(t, allowed)
			if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindFilePut, Path: target, Size: 8, Mode: 0o600}); err != nil {
				t.Fatal(err)
			}
			if reply, err := protocol.ReadMessage(conn); err != nil || reply.Kind != protocol.KindOK {
				t.Fatalf("put ack: reply=%+v err=%v", reply, err)
			}
			if err := protocol.WriteFrame(conn, protocol.FrameData, []byte("short")); err != nil {
				t.Fatal(err)
			}
			if err := protocol.WriteMessage(conn, tt.eof); err != nil {
				t.Fatal(err)
			}
			reply, err := protocol.ReadMessage(conn)
			if err != nil {
				t.Fatal(err)
			}
			if reply.Kind != protocol.KindError {
				t.Fatalf("upload result = %q, want %q", reply.Kind, protocol.KindError)
			}
			if got, err := os.ReadFile(target); err != nil || string(got) != "original" {
				t.Fatalf("failed upload changed target to %q (err=%v)", got, err)
			}
			assertNoUploadTemps(t, allowed)
		})
	}
}

func TestUploadRejectsDeclaredSizeOverLimit(t *testing.T) {
	allowed := t.TempDir()
	target := filepath.Join(allowed, "target.txt")
	if err := os.WriteFile(target, []byte("original"), 0o640); err != nil {
		t.Fatal(err)
	}
	conn := filePolicyConn(t, allowed)
	if err := protocol.WriteMessage(conn, protocol.Message{
		Kind: protocol.KindFilePut,
		Path: target,
		Size: protocol.MaxFileSize + 1,
		Mode: 0o600,
	}); err != nil {
		t.Fatal(err)
	}
	reply, err := protocol.ReadMessage(conn)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Kind != protocol.KindError {
		t.Fatalf("oversized upload reply = %q, want %q", reply.Kind, protocol.KindError)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "original" {
		t.Fatalf("oversized upload changed target to %q (err=%v)", got, err)
	}
	assertNoUploadTemps(t, allowed)
}

func TestUploadRejectsPayloadBeyondDeclaredSize(t *testing.T) {
	allowed := t.TempDir()
	target := filepath.Join(allowed, "target.txt")
	if err := os.WriteFile(target, []byte("original"), 0o640); err != nil {
		t.Fatal(err)
	}
	conn := filePolicyConn(t, allowed)
	if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindFilePut, Path: target, Size: 4, Mode: 0o600}); err != nil {
		t.Fatal(err)
	}
	if reply, err := protocol.ReadMessage(conn); err != nil || reply.Kind != protocol.KindOK {
		t.Fatalf("put ack: reply=%+v err=%v", reply, err)
	}
	if err := protocol.WriteFrame(conn, protocol.FrameData, []byte("12345")); err != nil {
		t.Fatal(err)
	}
	reply, err := protocol.ReadMessage(conn)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Kind != protocol.KindError {
		t.Fatalf("overlong payload reply = %q, want %q", reply.Kind, protocol.KindError)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "original" {
		t.Fatalf("overlong upload changed target to %q (err=%v)", got, err)
	}
	assertNoUploadTemps(t, allowed)
}

func TestSuccessfulUploadAtomicallyReplacesFileAndSetsMode(t *testing.T) {
	allowed := t.TempDir()
	target := filepath.Join(allowed, "target.txt")
	if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	payload := []byte("complete replacement")
	conn := filePolicyConn(t, allowed)
	if err := protocol.WriteMessage(conn, protocol.Message{
		Kind: protocol.KindFilePut,
		Path: target,
		Size: int64(len(payload)),
		Mode: 0o640,
	}); err != nil {
		t.Fatal(err)
	}
	if reply, err := protocol.ReadMessage(conn); err != nil || reply.Kind != protocol.KindOK {
		t.Fatalf("put ack: reply=%+v err=%v", reply, err)
	}
	if err := protocol.WriteFrame(conn, protocol.FrameData, payload); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "original" {
		t.Fatalf("target changed before commit to %q (err=%v)", got, err)
	}
	if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindEOF}); err != nil {
		t.Fatal(err)
	}
	reply, err := protocol.ReadMessage(conn)
	if err != nil || reply.Kind != protocol.KindOK || reply.Size != int64(len(payload)) {
		t.Fatalf("put result: reply=%+v err=%v", reply, err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != string(payload) {
		t.Fatalf("target content = %q (err=%v)", got, err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("target mode = %v, want 0640", info.Mode().Perm())
	}
	assertNoUploadTemps(t, allowed)
}

func assertNoUploadTemps(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		matches, err := filepath.Glob(filepath.Join(dir, ".wanctl-upload-*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("upload temp files remain: %v", matches)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
