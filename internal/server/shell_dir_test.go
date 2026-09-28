package server

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A session reports the directory its shell is actually in: where it started,
// and wherever a command has since moved it — including a name that would break
// any quoting a shell protocol might do on the way back.
func TestSessionReportsTheShellsDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	start := t.TempDir()
	s, err := NewShellSessionInDir("/bin/sh", start)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if got, err := s.CurrentDir(ctx); err != nil || got != start {
		t.Fatalf("start: CurrentDir = %q, %v; want %q", got, err, start)
	}
	odd := filepath.Join(t.TempDir(), "a dir;with 'quotes'\nand a newline")
	if err := os.Mkdir(odd, 0o755); err != nil {
		t.Fatal(err)
	}
	if code, err := s.ExecInDirContext(ctx, "cd -- "+quotePOSIXLiteral(odd), "", io.Discard); err != nil || code != 0 {
		t.Fatalf("cd: %d %v", code, err)
	}
	if got, err := s.CurrentDir(ctx); err != nil || got != odd {
		t.Fatalf("after cd: CurrentDir = %q, %v; want %q", got, err, odd)
	}
	// The session still works as before for the next command.
	var out strings.Builder
	if code, err := s.ExecInDirContext(ctx, "printf ok", "", &out); err != nil || code != 0 || strings.TrimSpace(out.String()) != "ok" {
		t.Fatalf("next command: %d %v %q", code, err, out.String())
	}
}

// A background job an earlier command left running still writes to the
// session's output stream. The directory must not be read from that stream.
func TestSessionDirectoryIgnoresBackgroundOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	start := t.TempDir()
	s, err := NewShellSessionInDir("/bin/sh", start)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.ExecInDirContext(ctx, "(i=0; while [ $i -lt 200 ]; do echo /elsewhere; i=$((i+1)); sleep 0.005; done) &", "", io.Discard); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		if got, err := s.CurrentDir(ctx); err != nil || got != start {
			t.Fatalf("CurrentDir = %q, %v; want %q", got, err, start)
		}
	}
}

// A directory that no longer exists cannot be named. That is reported as an
// error, not as some other directory, so the caller can treat it as unknown.
func TestSessionDirectoryUnknownWhenRemoved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	gone := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := NewShellSessionInDir("/bin/sh", gone)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if got, err := s.CurrentDir(ctx); err == nil && got != gone {
		t.Fatalf("CurrentDir of a removed directory = %q, want an error or %q", got, gone)
	}
}

// The PowerShell form cannot run here, so check what it is made of: the
// location comes from the engine rather than a command a script could have
// redefined, a non-filesystem location writes nothing, and the data path is
// a quoted literal.
func TestCurrentDirCommandShapes(t *testing.T) {
	ps := currentDirCommand("windows", `C:\Temp\wanctl-pwd-'x`)
	for _, want := range []string{"$ExecutionContext.SessionState.Path.CurrentLocation", "'FileSystem'", ".ProviderPath", `'C:\Temp\wanctl-pwd-''x'`} {
		if !strings.Contains(ps, want) {
			t.Errorf("PowerShell command %q lacks %q", ps, want)
		}
	}
	sh := currentDirCommand("linux", "/tmp/wanctl-pwd-'x")
	for _, want := range []string{"command pwd", ">|", `'/tmp/wanctl-pwd-'"'"'x'`} {
		if !strings.Contains(sh, want) {
			t.Errorf("POSIX command %q lacks %q", sh, want)
		}
	}
}
