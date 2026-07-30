//go:build lark

package lark

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestLiveConsumer opens a real Feishu long connection and logs every card
// callback until the deadline. It exists for two reasons that unit tests cannot
// cover:
//
//  1. The Feishu console's 回调配置 page will only switch an app to long-connection
//     mode once it can see a live SDK client, and its 验证 button checks the same
//     thing. So a client has to be running while someone saves that setting.
//  2. It is the only way to observe a real card.action.trigger arriving — the
//     callback half of this feature is otherwise verified solely against the SDK's
//     source and our own fakes.
//
// Run it for three minutes and tap a card button in Feishu:
//
//	WANCTL_LARK_APP_ID=... WANCTL_LARK_APP_SECRET=... WANCTL_LARK_LIVE_SECONDS=180 \
//	  go test -tags lark -v -run TestLiveConsumer ./internal/lark/ -timeout 10m
//
// A clean exit at the deadline with no callback logged is NOT success — it means
// nothing was tapped, or 回调配置 is still off (in which case the connection is
// established and silent, with no error and no preflight to warn you).
func TestLiveConsumer(t *testing.T) {
	appID, secret := os.Getenv("WANCTL_LARK_APP_ID"), os.Getenv("WANCTL_LARK_APP_SECRET")
	seconds, _ := strconv.Atoi(os.Getenv("WANCTL_LARK_LIVE_SECONDS"))
	if appID == "" || secret == "" || seconds <= 0 {
		t.Skip("set WANCTL_LARK_APP_ID, WANCTL_LARK_APP_SECRET and WANCTL_LARK_LIVE_SECONDS to run")
	}

	got := 0
	consumer, err := NewConsumer(appID, secret, func(_ context.Context, a CardAction) CardReply {
		got++
		t.Logf("CALLBACK #%d verdict=%q nonce=%q chat=%q message=%q event=%q operator=%q",
			got, a.Verdict, a.Nonce, a.ChatID, a.MessageID, a.EventID, a.OperatorOpenID)
		return CardReply{ToastText: "收到（联调探测，不会真的执行）"}
	})
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(seconds)*time.Second)
	defer cancel()
	t.Logf("long connection opening; will listen for %ds", seconds)
	if err := consumer.Run(ctx); err != nil && ctx.Err() == nil {
		t.Fatalf("Run: %v", err)
	}
	t.Logf("listened %ds, received %d callback(s)", seconds, got)
}
