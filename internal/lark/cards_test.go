package lark

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"wanctl/internal/console"
	"wanctl/internal/policy"
)

func TestApprovalAndResolvedCardsAreSafeV2Cards(t *testing.T) {
	pending := console.Pending{
		ID:   "pending-1",
		Kind: "exec",
		Cmd:  "deploy.sh --token=abcd1234efgh5678",
		Path: "/srv?access_token=pathsecret",
		Cwd:  "/srv/API_KEY=cwdsecret",
		Peer: "alice@macbook (SHA256:kP9x)",
	}
	cards := []map[string]any{
		ApprovalCard("macbox", pending, "nonce", "https://wanctl.example/#/devices/macbox", 3*time.Minute),
		ResolvedCard("macbox", pending, "已允许", "lark:owner@example.test"),
	}
	for i, card := range cards {
		raw, err := json.Marshal(card)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, secret := range []string{"abcd1234efgh5678", "pathsecret", "cwdsecret"} {
			if strings.Contains(text, secret) {
				t.Errorf("card %d contains secret %q: %s", i, secret, text)
			}
		}
		if card["schema"] != "2.0" {
			t.Errorf("card %d schema = %#v", i, card["schema"])
		}
		config, ok := card["config"].(map[string]any)
		if !ok || config["enable_forward"] != false {
			t.Errorf("card %d enable_forward = %#v", i, config["enable_forward"])
		}
		assertMarkdownHasNoFontColor(t, card)
	}
	if strings.Contains(string(mustJSON(t, cards[1])), `"tag":"button"`) {
		t.Fatal("ResolvedCard contains a button")
	}
}

// TestApprovalCardVerdictButtons pins the button set per request kind. The logs
// case is the one that matters: console.Service.Decide downgrades `a` to a plain
// one-shot allow for policy.KindLogs, so offering 「允许并记住此目录」 there would
// render a button that does not do what its label says.
func TestApprovalCardVerdictButtons(t *testing.T) {
	for _, tc := range []struct {
		kind string
		want []string
	}{
		{"exec", []string{"y", "n", "a", "g"}},
		{"write", []string{"y", "n", "a", "g"}},
		{string(policy.KindLogs), []string{"y", "n", "g"}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			card := ApprovalCard("macbox", console.Pending{Kind: tc.kind, Cmd: "true"},
				"the-nonce", "https://wanctl.example/#/devices/macbox", time.Minute)
			verdicts, nonces := callbackValues(t, card)

			if strings.Join(verdicts, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("verdict buttons: got %v, want %v", verdicts, tc.want)
			}
			for _, v := range verdicts {
				if !Verdicts[v] {
					t.Fatalf("button carries verdict %q, outside the accepted set", v)
				}
			}
			for _, n := range nonces {
				if n != "the-nonce" {
					t.Fatalf("button carries nonce %q, want the-nonce", n)
				}
			}
		})
	}
}

// TestCardsNeverEmitBrTags guards a bug that only a real client showed: a <br>
// nested inside <font> is accepted by the API (code 0) and then rendered as
// literal text, so the card read "…(SHA256:kP9x)<br>工作目录 /srv/app". Newlines
// work in every position, so no template should emit a <br> at all.
func TestCardsNeverEmitBrTags(t *testing.T) {
	pending := console.Pending{Kind: "exec", Cmd: "true", Path: "/srv/x", Cwd: "/srv", Peer: "someone"}
	pairing := console.PendingPairing{FP: "SHA256:fp", Name: "c", Label: "l"}
	for name, card := range map[string]map[string]any{
		"approval":         ApprovalCard("macbox", pending, "n", "https://p", time.Minute),
		"resolved":         ResolvedCard("macbox", pending, "已允许", "lark:a@b.c"),
		"pairing":          PairingCard("macbox", pairing, "n", "https://p", true),
		"resolved pairing": ResolvedPairingCard("macbox", pairing, "已信任", "lark:a@b.c"),
		"action failed":    ActionFailedCard("该审批已失效"),
	} {
		if raw := string(mustJSON(t, card)); strings.Contains(raw, "<br>") {
			t.Errorf("%s card emits a <br>, which renders literally inside <font>: %s", name, raw)
		}
	}
}

func TestHumanWait(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{30 * time.Second, "30 秒"},
		{time.Minute, "1 分钟"},
		{3 * time.Minute, "3 分钟"},
		{90 * time.Second, "1 分 30 秒"},
	} {
		if got := humanWait(tc.in); got != tc.want {
			t.Errorf("humanWait(%s): got %q, want %q", tc.in, got, tc.want)
		}
	}
}

// callbackValues walks the rendered card and returns the verdict and nonce of
// every callback button, in document order.
func callbackValues(t *testing.T, card map[string]any) (verdicts, nonces []string) {
	t.Helper()
	var tree any
	if err := json.Unmarshal(mustJSON(t, card), &tree); err != nil {
		t.Fatal(err)
	}
	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			if v["type"] == "callback" {
				if val, ok := v["value"].(map[string]any); ok {
					a, _ := val["a"].(string)
					nonce, _ := val["n"].(string)
					verdicts = append(verdicts, a)
					nonces = append(nonces, nonce)
				}
			}
			// Iterate the ordered element lists rather than the map so document
			// order is preserved; map ranging would shuffle the buttons.
			if body, ok := v["body"].(map[string]any); ok {
				walk(body)
			}
			for _, key := range []string{"elements", "columns", "behaviors"} {
				if child, ok := v[key]; ok {
					walk(child)
				}
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(tree)
	return verdicts, nonces
}

func TestPairingCardActionsFollowSwitch(t *testing.T) {
	pairing := console.PendingPairing{FP: "SHA256:full-fingerprint", Name: "controller", Label: "release operator"}
	without := string(mustJSON(t, PairingCard("macbox", pairing, "nonce", "https://wanctl.example", false)))
	with := string(mustJSON(t, PairingCard("macbox", pairing, "nonce", "https://wanctl.example", true)))
	if strings.Contains(without, `"type":"callback"`) {
		t.Fatalf("notify-only pairing card contains callback: %s", without)
	}
	if strings.Count(with, `"type":"callback"`) != 2 {
		t.Fatalf("interactive pairing callback count = %d, want 2: %s", strings.Count(with, `"type":"callback"`), with)
	}
	if !strings.Contains(with, pairing.FP) {
		t.Fatalf("pairing card omitted full fingerprint: %s", with)
	}
	verdicts, _ := callbackValues(t, PairingCard("macbox", pairing, "nonce", "https://wanctl.example", true))
	if !reflect.DeepEqual(verdicts, []string{"y", "n"}) {
		t.Fatalf("pairing verdicts = %v, want device-native y/n", verdicts)
	}
	assertMarkdownHasNoFontColor(t, PairingCard("macbox", pairing, "nonce", "https://wanctl.example", true))
}

func TestResolvedPairingAndActionFailureCardsHaveNoActions(t *testing.T) {
	pairing := console.PendingPairing{FP: "SHA256:full-fingerprint", Name: "controller", Label: "release operator"}
	for name, card := range map[string]map[string]any{
		"resolved pairing": ResolvedPairingCard("macbox", pairing, "已信任", "lark:owner@example.test"),
		"action failure":   ActionFailedCard("该审批已失效"),
	} {
		raw := string(mustJSON(t, card))
		if strings.Contains(raw, `"type":"callback"`) {
			t.Fatalf("%s card contains callback: %s", name, raw)
		}
	}
}

func assertMarkdownHasNoFontColor(t *testing.T, value any) {
	t.Helper()
	switch value := value.(type) {
	case map[string]any:
		if value["tag"] == "markdown" {
			if _, exists := value["font_color"]; exists {
				t.Errorf("markdown contains font_color: %#v", value)
			}
		}
		for _, child := range value {
			assertMarkdownHasNoFontColor(t, child)
		}
	case []any:
		for _, child := range value {
			assertMarkdownHasNoFontColor(t, child)
		}
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
