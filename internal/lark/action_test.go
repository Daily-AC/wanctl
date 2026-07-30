package lark

import "testing"

func TestParseAction(t *testing.T) {
	got, err := ParseAction(
		map[string]any{"a": "y", "n": "nonce-1"},
		"ou_operator", "oc_chat", "om_message", "event-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	want := CardAction{
		Verdict:        "y",
		Nonce:          "nonce-1",
		OperatorOpenID: "ou_operator",
		ChatID:         "oc_chat",
		MessageID:      "om_message",
		EventID:        "event-1",
	}
	if got != want {
		t.Fatalf("ParseAction = %#v, want %#v", got, want)
	}
}

func TestParseActionRejectsUnknownVerdict(t *testing.T) {
	if _, err := parseTestAction(map[string]any{"a": "allow", "n": "nonce-1"}); err == nil {
		t.Fatal("ParseAction accepted a verdict outside Verdicts")
	}
}

func TestParseActionRejectsMissingFields(t *testing.T) {
	for _, key := range []string{"a", "n"} {
		t.Run(key, func(t *testing.T) {
			value := map[string]any{"a": "y", "n": "nonce-1"}
			delete(value, key)
			if _, err := parseTestAction(value); err == nil {
				t.Fatalf("ParseAction accepted payload missing %q", key)
			}
		})
	}
}

func TestParseActionRejectsEmptyVerdict(t *testing.T) {
	if _, err := parseTestAction(map[string]any{"a": "", "n": "nonce-1"}); err == nil {
		t.Fatal("ParseAction accepted an empty verdict")
	}
}

func parseTestAction(value map[string]any) (CardAction, error) {
	return ParseAction(value, "ou_operator", "oc_chat", "om_message", "event-1")
}
