package lark

import (
	"context"
	"os"
	"testing"
	"time"

	"wanctl/internal/console"
)

// TestLiveSendAndResolveCard exercises the real Feishu API: it sends a full
// approval card to a real inbox and then patches it into its resolved state.
// httptest covers the request shapes, but only this proves the card JSON is
// something Feishu will actually accept — its validator rejects unknown
// properties outright (a `font_color` on a markdown element is a hard 230099),
// so a template that passes unit tests can still fail for every real user.
//
// Env-gated like internal/client's TestLiveRemoteConsole:
//
//	WANCTL_LARK_APP_ID=... WANCTL_LARK_APP_SECRET=... \
//	WANCTL_LARK_TEST_EMAIL=you@example.com go test -run TestLive ./internal/lark/
//
// It leaves one resolved card in that chat, clearly marked as a probe.
func TestLiveSendAndResolveCard(t *testing.T) {
	appID := os.Getenv("WANCTL_LARK_APP_ID")
	secret := os.Getenv("WANCTL_LARK_APP_SECRET")
	to := os.Getenv("WANCTL_LARK_TEST_EMAIL")
	if appID == "" || secret == "" || to == "" {
		t.Skip("set WANCTL_LARK_APP_ID, WANCTL_LARK_APP_SECRET and WANCTL_LARK_TEST_EMAIL to run")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := NewClient(appID, secret)

	pending := console.Pending{
		ID:      "live-probe",
		Kind:    "exec",
		Cmd:     "cd /srv/app && ./deploy.sh --token=abcd1234efgh5678",
		Cwd:     "/srv/app",
		Peer:    "***REMOVED***@macbook (SHA256:kP9x)",
		Created: time.Now(),
	}
	card := ApprovalCard("macbox", pending, "live-probe-nonce",
		"https://wanctl.***REMOVED***.***REMOVED***.com/#/devices/macbox", 3*time.Minute)

	messageID, chatID, err := c.SendCard(ctx, to, card)
	if err != nil {
		t.Fatalf("SendCard: %v", err)
	}
	if messageID == "" || chatID == "" {
		t.Fatalf("SendCard returned message_id=%q chat_id=%q; both are required "+
			"(chat_id is what binds a nonce to one conversation)", messageID, chatID)
	}
	t.Logf("sent %s to %s", messageID, chatID)

	// Set WANCTL_LARK_KEEP_CARD=1 to leave the buttons in place, so a human can
	// tap one and a TestLiveConsumer run can observe the callback. Without it the
	// card is resolved immediately and the probe leaves no actionable message
	// sitting in someone's chat.
	if os.Getenv("WANCTL_LARK_KEEP_CARD") == "1" {
		t.Logf("card left actionable at %s — tap a button to exercise the callback", messageID)
		return
	}
	if err := c.UpdateCard(ctx, messageID,
		ResolvedCard("macbox", pending, "已允许（联调探测，可忽略）", "lark:"+to)); err != nil {
		t.Fatalf("UpdateCard: %v", err)
	}
}
