//go:build lark

package lark

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	"github.com/larksuite/oapi-sdk-go/v3/ws"
)

// Consumer receives Feishu card callbacks over the SDK's long connection.
type Consumer struct {
	client *ws.Client
}

// NewConsumer constructs a long-connection consumer with one card callback
// registration. The SDK owns reconnection once Run starts.
func NewConsumer(appID, appSecret string, h ActionHandler) (*Consumer, error) {
	if appID == "" {
		return nil, errors.New("lark consumer: app ID is required")
	}
	if appSecret == "" {
		return nil, errors.New("lark consumer: app secret is required")
	}
	if h == nil {
		return nil, errors.New("lark consumer: action handler is required")
	}

	d := dispatcher.NewEventDispatcher("", "")
	d.OnP2CardActionTrigger(func(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
		action, err := actionFromEvent(event)
		if err != nil {
			return toastResponse("卡片操作无效：" + err.Error()), nil
		}
		return toastResponse(replyWithin(ctx, handlerBudget, h, action)), nil
	})
	return &Consumer{
		client: ws.NewClient(
			appID,
			appSecret,
			ws.WithEventHandler(d),
			ws.WithAutoReconnect(true),
		),
	}, nil
}

// Run starts the Feishu long connection. Reconnection is handled by the SDK.
func (c *Consumer) Run(ctx context.Context) error {
	if c == nil || c.client == nil {
		return errors.New("lark consumer is not initialized")
	}
	return c.client.Start(ctx)
}

func actionFromEvent(event *callback.CardActionTriggerEvent) (CardAction, error) {
	if event == nil || event.Event == nil {
		return CardAction{}, errors.New("missing callback event")
	}
	if event.Event.Operator == nil {
		return CardAction{}, errors.New("missing callback operator")
	}
	if event.Event.Context == nil {
		return CardAction{}, errors.New("missing callback context")
	}
	if event.Event.Action == nil {
		return CardAction{}, errors.New("missing callback action")
	}
	if event.EventV2Base == nil || event.EventV2Base.Header == nil {
		return CardAction{}, errors.New("missing callback header")
	}
	action, err := ParseAction(
		event.Event.Action.Value,
		event.Event.Operator.OpenID,
		event.Event.Context.OpenChatID,
		event.Event.Context.OpenMessageID,
		event.EventV2Base.Header.EventID,
	)
	if err != nil {
		return CardAction{}, fmt.Errorf("parse callback: %w", err)
	}
	return action, nil
}

// handlerBudget is how long the callback waits for the handler's toast text.
// Feishu closes the callback at ~3s and then redelivers the event, which would
// drive a second decision attempt for the same tap (event-ID dedup catches it,
// but silently). Answering early keeps the contract even if a handler is
// mistakenly written to block on the device RPC.
const handlerBudget = 2 * time.Second

// replyWithin runs h and returns its toast, or a placeholder if h outlives the
// budget. h is NOT cancelled on timeout: by then it may already be committing a
// decision, and abandoning that half-way would be worse than a vague toast. Its
// own card update remains the authoritative outcome the user sees.
func replyWithin(ctx context.Context, budget time.Duration, h ActionHandler, a CardAction) string {
	done := make(chan string, 1)
	go func() { done <- h(ctx, a).ToastText }()
	select {
	case text := <-done:
		return text
	case <-time.After(budget):
		return "已收到，正在处理，请稍后查看卡片"
	}
}

func toastResponse(text string) *callback.CardActionTriggerResponse {
	if text == "" {
		// An empty toast renders as a blank bubble; say nothing instead.
		return &callback.CardActionTriggerResponse{}
	}
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: "info", Content: text},
	}
}
