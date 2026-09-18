package console

import (
	"strings"
	"testing"
	"time"

	"wanctl/internal/policy"
	"wanctl/internal/script"
)

// ask runs one request against a service with a front-end attached, and returns
// the card a portal would draw for it.
func ask(t *testing.T, s *Service, req policy.Request) Pending {
	t.Helper()
	changed, cancel := s.Subscribe()
	defer cancel()
	go s.Ask(req)
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the queue")
	}
	pend := s.State().Pending
	if len(pend) != 1 {
		t.Fatalf("pending = %+v, want one entry", pend)
	}
	s.Decide(pend[0].ID, "n")
	return pend[0]
}

// The portal card is the second place a command is read before someone decides
// about it, and it must not abbreviate anything that is not a script. A command
// shaped like a token — `script:sh:<16 hex>; printf …` — was drawn as just the
// token, so the half that runs was invisible (review of #108, 2026-09-18).
func TestPendingCardShowsASpoofedTokenWhole(t *testing.T) {
	const spoof = "script:sh:0123456789abcdef; printf REVIEW_EXECUTED"
	s := New(&policy.Engine{}, nil, Info{})
	card := ask(t, s, policy.Request{Kind: policy.KindExecElevated, Cmd: spoof})
	if card.Cmd != spoof {
		t.Fatalf("card drew %q, want the whole command %q", card.Cmd, spoof)
	}
	if strings.Contains(card.Cmd, "…") {
		t.Fatalf("card abbreviated a command that is not a script: %q", card.Cmd)
	}
}

// A real script still shows as the abbreviated token, and what is drawn is a
// visible prefix of the rule the device would write.
func TestPendingCardAbbreviatesARealScript(t *testing.T) {
	cmd, err := script.Command(script.POSIX, []byte("pm install -r /sdcard/app.apk\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(&policy.Engine{}, nil, Info{})
	card := ask(t, s, policy.Request{Kind: policy.KindExecElevated, Cmd: cmd})
	full, _ := script.Canonical(cmd)
	if card.Cmd == full || card.Cmd == cmd {
		t.Fatalf("card drew %q, want the abbreviated token", card.Cmd)
	}
	if !strings.HasPrefix(full, strings.TrimSuffix(card.Cmd, "…")) {
		t.Fatalf("card %q is not a visible prefix of the rule %q", card.Cmd, full)
	}
}
