//go:build lark

package lark

import (
	"context"
	"testing"
	"time"
)

func TestNewConsumerTaggedBuild(t *testing.T) {
	consumer, err := NewConsumer("cli_test", "secret", func(context.Context, CardAction) CardReply {
		return CardReply{ToastText: "received"}
	})
	if err != nil {
		t.Fatal(err)
	}
	if consumer == nil || consumer.client == nil {
		t.Fatal("NewConsumer returned an uninitialized consumer")
	}
}

// TestReplyWithinAnswersBeforeFeishuGivesUp covers the case where a handler is
// written to block (e.g. it waits on the device RPC instead of deciding
// asynchronously). Feishu closes the callback at ~3s and redelivers, so the
// callback must answer on its own rather than ride the handler.
func TestReplyWithinAnswersBeforeFeishuGivesUp(t *testing.T) {
	released := make(chan struct{})
	defer close(released)

	slow := func(context.Context, CardAction) CardReply {
		<-released // outlives the budget, like a synchronous device RPC would
		return CardReply{ToastText: "too late"}
	}
	start := time.Now()
	got := replyWithin(context.Background(), 20*time.Millisecond, slow, CardAction{})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("replyWithin waited %s for a blocked handler", elapsed)
	}
	if got == "" || got == "too late" {
		t.Fatalf("expected a placeholder toast, got %q", got)
	}
}

func TestReplyWithinReturnsHandlerToast(t *testing.T) {
	got := replyWithin(context.Background(), time.Second,
		func(context.Context, CardAction) CardReply { return CardReply{ToastText: "已允许"} },
		CardAction{})
	if got != "已允许" {
		t.Fatalf("toast: got %q, want 已允许", got)
	}
}

func TestToastResponseOmitsEmptyBubble(t *testing.T) {
	if resp := toastResponse(""); resp.Toast != nil {
		t.Fatal("empty toast text still produced a toast bubble")
	}
	if resp := toastResponse("x"); resp.Toast == nil || resp.Toast.Content != "x" {
		t.Fatal("non-empty toast text was dropped")
	}
}
