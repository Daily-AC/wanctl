package eventlog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRedactText(t *testing.T) {
	got := RedactText("deploy --token=abcd1234efgh5678 --region us-east-1")
	if strings.Contains(got, "abcd1234efgh5678") {
		t.Fatalf("RedactText retained secret: %q", got)
	}
	if !strings.Contains(got, "--token=[REDACTED]") || !strings.Contains(got, "--region us-east-1") {
		t.Fatalf("RedactText lost useful context: %q", got)
	}
}

func TestAppendRedactsSecretsBeforeWritingJSONL(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	l, err := Open("events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	details := []string{
		"deploy --token raw-token-value --region us-east-1",
		"db-migrate --password=hunter2 --dry-run",
		`curl -H 'Authorization: Bearer header-secret' https://api.example.test/v1`,
		"fetch https://api.example.test/items?token=url-secret&limit=10",
	}
	for _, detail := range details {
		if err := l.Append(Event{Type: "exec", Detail: detail}); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"raw-token-value", "hunter2", "header-secret", "url-secret"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("secret %q was written to disk: %s", secret, raw)
		}
	}
	for _, executable := range []string{"deploy", "db-migrate", "curl", "fetch"} {
		if !strings.Contains(string(raw), executable) {
			t.Fatalf("audit summary lost executable %q: %s", executable, raw)
		}
	}
	if strings.Count(string(raw), "[REDACTED]") < len(details) {
		t.Fatalf("expected one or more redactions per event: %s", raw)
	}
	if !strings.Contains(string(raw), "limit=10") || !strings.Contains(string(raw), "region us-east-1") {
		t.Fatalf("redaction removed non-sensitive audit context: %s", raw)
	}
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	for sc.Scan() {
		var event Event
		if err := json.Unmarshal(sc.Bytes(), &event); err != nil {
			t.Fatalf("redaction broke JSONL: %v: %q", err, sc.Bytes())
		}
	}
}

func TestAppendRotatesAndBoundsBackups(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	l, err := Open("events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	l.maxBytes = 4 << 10
	payload := strings.Repeat("x", 1024)
	for i := 0; i < 100; i++ {
		if err := l.Append(Event{Type: "exec", Detail: payload}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(l.Path() + ".1"); err != nil {
		t.Fatalf("log did not rotate after exceeding its size limit: %v", err)
	}
	matches, err := filepath.Glob(l.Path() + "*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) > 4 {
		t.Fatalf("log retained %d files, want active + at most 3 backups: %v", len(matches), matches)
	}
	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 600", path, info.Mode().Perm())
		}
		if info.Size() > l.maxBytes {
			t.Fatalf("%s size = %d, exceeds %d", path, info.Size(), l.maxBytes)
		}
		data, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(data)
		for sc.Scan() {
			if !json.Valid(sc.Bytes()) {
				t.Fatalf("rotation broke JSONL in %s: %q", path, sc.Bytes())
			}
		}
		data.Close()
	}
}

func TestAppendBoundsOversizedEvent(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	l, err := Open("events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append(Event{Type: "exec", Detail: strings.Repeat("x", 2<<20)}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > l.maxBytes {
		t.Fatalf("single event produced %d-byte segment, exceeds %d", info.Size(), l.maxBytes)
	}
}

func TestAppendReadFilter(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	l, err := Open("events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	code := 0
	l.Append(Event{Type: "connect", PeerName: "ctl"})
	l.Append(Event{Type: "exec", Detail: "git status", Decision: "approved", Exit: &code})
	l.Append(Event{Type: "exec", Detail: "rm -rf /", Decision: "denied"})
	l.Append(Event{Type: "file", Detail: "/data/a.txt", Decision: "pre-approved", Bytes: 42})

	all, err := l.Read(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("want 4 events, got %d", len(all))
	}

	execs, _ := l.Read(Filter{Type: "exec"})
	if len(execs) != 2 {
		t.Fatalf("want 2 exec events, got %d", len(execs))
	}

	grep, _ := l.Read(Filter{Grep: "rm -rf"})
	if len(grep) != 1 || grep[0].Decision != "denied" {
		t.Fatalf("grep failed: %+v", grep)
	}

	last, _ := l.Read(Filter{Limit: 1})
	if len(last) != 1 || last[0].Type != "file" {
		t.Fatalf("limit failed: %+v", last)
	}
}

func TestReadMissingFileIsEmpty(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	l, _ := Open("events.jsonl")
	evs, err := l.Read(Filter{})
	if err != nil || len(evs) != 0 {
		t.Fatalf("expected empty, got %v err=%v", evs, err)
	}
	_ = time.Now
}
