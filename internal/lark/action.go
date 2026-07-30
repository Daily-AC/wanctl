package lark

import (
	"context"
	"fmt"
)

// CardAction is the SDK-independent form of a Feishu card callback.
type CardAction struct {
	Verdict        string
	Nonce          string
	OperatorOpenID string
	ChatID         string
	MessageID      string
	EventID        string
}

// CardReply is the immediate response sent for a card callback. The caller is
// responsible for performing the decision and card update asynchronously.
type CardReply struct {
	ToastText string
}

// ActionHandler handles one parsed card callback within Feishu's response
// window.
type ActionHandler func(ctx context.Context, a CardAction) CardReply

// ParseAction validates a callback button payload and attaches its event
// context. Only verdicts rendered by this package may reach the caller.
func ParseAction(value map[string]any, operatorOpenID, chatID, messageID, eventID string) (CardAction, error) {
	verdict, err := actionString(value, "a")
	if err != nil {
		return CardAction{}, err
	}
	if !Verdicts[verdict] {
		return CardAction{}, fmt.Errorf("invalid card action verdict %q", verdict)
	}
	nonce, err := actionString(value, "n")
	if err != nil {
		return CardAction{}, err
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"operator open ID", operatorOpenID},
		{"chat ID", chatID},
		{"message ID", messageID},
		{"event ID", eventID},
	} {
		if field.value == "" {
			return CardAction{}, fmt.Errorf("card action missing %s", field.name)
		}
	}
	return CardAction{
		Verdict:        verdict,
		Nonce:          nonce,
		OperatorOpenID: operatorOpenID,
		ChatID:         chatID,
		MessageID:      messageID,
		EventID:        eventID,
	}, nil
}

func actionString(value map[string]any, key string) (string, error) {
	raw, exists := value[key]
	if !exists {
		return "", fmt.Errorf("card action missing %q", key)
	}
	text, ok := raw.(string)
	if !ok || text == "" {
		return "", fmt.Errorf("card action field %q must be a non-empty string", key)
	}
	return text, nil
}
