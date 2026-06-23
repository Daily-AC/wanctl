package console

import (
	"testing"
	"time"

	"wanctl/internal/policy"
)

func newSvc(t *testing.T) *Service {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	eng, err := policy.Open("rules.json", policy.ModeNormal)
	if err != nil {
		t.Fatal(err)
	}
	return New(eng, nil, Info{Device: "dev1"})
}

func TestAskBlocksUntilDecide(t *testing.T) {
	s := newSvc(t)
	s.timeout = time.Second
	done := make(chan policy.Decision, 1)
	go func() { done <- s.Ask(policy.Request{Kind: policy.KindExec, Cmd: "echo hi"}) }()

	// the pending request shows up in State
	var id string
	for i := 0; i < 50; i++ {
		if p := s.State().Pending; len(p) == 1 {
			id = p[0].ID
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("pending never appeared")
	}
	if !s.Decide(id, "y") {
		t.Fatal("decide missed")
	}
	d := <-done
	if !d.Allow {
		t.Fatalf("want allow, got %+v", d)
	}
}

func TestAskTimeoutDenies(t *testing.T) {
	s := newSvc(t)
	s.timeout = 50 * time.Millisecond
	d := s.Ask(policy.Request{Kind: policy.KindExec, Cmd: "x"})
	if d.Allow {
		t.Fatal("timeout should deny")
	}
}

func TestSubscribeNotifiedOnPending(t *testing.T) {
	s := newSvc(t)
	s.timeout = time.Second
	ch, cancel := s.Subscribe()
	defer cancel()
	go s.Ask(policy.Request{Kind: policy.KindExec, Cmd: "y"})
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("no notification")
	}
}

func TestDecideVerdicts(t *testing.T) {
	s := newSvc(t)
	s.timeout = time.Second
	for _, tc := range []struct {
		v            string
		allow, remem bool
		scope        policy.Scope
	}{
		{"y", true, false, ""},
		{"a", true, true, policy.ScopeDir},
		{"g", true, true, policy.ScopeGlobal},
		{"n", false, false, ""},
	} {
		got := make(chan policy.Decision, 1)
		go func() { got <- s.Ask(policy.Request{Kind: policy.KindExec, Cmd: "z"}) }()
		var id string
		for id == "" {
			if p := s.State().Pending; len(p) > 0 {
				id = p[0].ID
			}
		}
		s.Decide(id, tc.v)
		d := <-got
		if d.Allow != tc.allow || d.Remember != tc.remem || d.Scope != tc.scope {
			t.Fatalf("verdict %q -> %+v", tc.v, d)
		}
	}
}
