package eventlog

import (
	"testing"
	"time"
)

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
