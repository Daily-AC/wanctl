package serverlog

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRedact(t *testing.T) {
	in := `token=abc123 secret: hush Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.abc api_key="key123" blob=YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoxMjM0NTY=`
	got := Redact(in)
	for _, secret := range []string{"abc123", "hush", "eyJhbGciOiJIUzI1NiJ9.abc", "key123", "YWJjZGVm"} {
		if strings.Contains(got, secret) {
			t.Errorf("redacted output contains %q: %q", secret, got)
		}
	}
}

func TestReadRedactsBeforeGrepAndTruncates(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	b := New(3, 1024)
	b.now = func() time.Time { return now }
	b.AppendAt(now, "first token=needle")
	b.AppendAt(now, "second lark")
	b.AppendAt(now, "third lark")

	secretProbe := b.Read(Query{Service: "portal", Since: time.Minute, Limit: 2, Grep: "needle"})
	if len(secretProbe.Logs) != 0 {
		t.Fatalf("grep observed a redacted secret: %#v", secretProbe.Logs)
	}
	got := b.Read(Query{Service: "portal", Since: time.Minute, Limit: 1, Grep: "lark"})
	if len(got.Logs) != 1 || got.Logs[0] != "third lark" || !got.Truncated {
		t.Fatalf("read = %#v", got)
	}
}

func TestParseQueryDefaultsAndCapsLimit(t *testing.T) {
	q, err := ParseQuery(url.Values{"service": {"relay"}, "limit": {"9000"}})
	if err != nil {
		t.Fatal(err)
	}
	if q.Since != 15*time.Minute || q.Limit != MaxLimit {
		t.Fatalf("query = %#v", q)
	}
}

func TestPendingLineIsBounded(t *testing.T) {
	b := New(10, 8)
	_, _ = b.Write([]byte("0123456789"))
	if len(b.pending) != 8 || string(b.pending) != "23456789" {
		t.Fatalf("pending = %q", b.pending)
	}
}
