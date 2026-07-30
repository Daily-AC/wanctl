package lark

import (
	"fmt"
	"time"

	"wanctl/internal/console"
	"wanctl/internal/eventlog"
	"wanctl/internal/policy"
)

// ApprovalCard renders a command, file, or log-read approval with the device's
// full set of verdict buttons and a portal deep link. wait is the device's
// current approval timeout, stated on the card so the person deciding knows how
// long they actually have.
func ApprovalCard(device string, pending console.Pending, nonce, portalURL string, wait time.Duration) map[string]any {
	elements := pendingElements(pending)
	elements = append(elements, map[string]any{"tag": "hr"})
	elements = append(elements, verdictRows(approvalVerdicts(pending.Kind), nonce)...)
	elements = append(elements, portalButton(portalURL))
	return cardShell(
		"wanctl 审批请求："+device,
		"审批请求 · "+device,
		"规则未命中，等待放行（最长 "+humanWait(wait)+"）",
		"orange",
		"warning_colorful",
		pending.Kind,
		elements,
	)
}

// verdictButton is one decision the card offers. action carries the device's own
// verdict character, so the callback handler needs no translation table — only a
// whitelist check against Verdicts.
type verdictButton struct {
	verdict string
	text    string
	style   string
}

// Verdicts is the set of verdict characters a card button may carry. A callback
// must reject anything outside it rather than forwarding an arbitrary string to
// the device (which would deny by default, but silently).
var Verdicts = map[string]bool{"y": true, "a": true, "g": true, "n": true}

// approvalVerdicts picks the buttons for a pending request.
//
// `a` (allow + remember this directory) is deliberately omitted for log reads:
// console.Service.Decide downgrades `a` to a plain one-shot allow for
// policy.KindLogs, because the engine will not remember a directory scope for
// them. A button labelled 「允许并记住此目录」 that quietly does not remember is
// worse than no button.
func approvalVerdicts(kind string) []verdictButton {
	buttons := []verdictButton{
		{"y", "允许一次", "primary_filled"},
		{"n", "拒绝", "danger"},
	}
	if kind != string(policy.KindLogs) {
		buttons = append(buttons, verdictButton{"a", "允许并记住此目录", "default"})
	}
	return append(buttons, verdictButton{"g", "全局允许", "default"})
}

// verdictRows lays the buttons out two per row: the one-shot allow and the deny
// come first as the common case, the durable grants below them.
func verdictRows(buttons []verdictButton, nonce string) []any {
	rows := make([]any, 0, (len(buttons)+1)/2)
	for i := 0; i < len(buttons); i += 2 {
		end := i + 2
		if end > len(buttons) {
			end = len(buttons)
		}
		cols := make([]any, 0, 2)
		for _, b := range buttons[i:end] {
			cols = append(cols, map[string]any{
				"tag": "column", "width": "weighted", "weight": 1,
				"elements": []any{callbackButton(b.text, b.style, nonce, b.verdict)},
			})
		}
		rows = append(rows, map[string]any{
			"tag": "column_set", "horizontal_spacing": "medium", "columns": cols,
		})
	}
	return rows
}

// humanWait renders a timeout the way a person reading a phone notification
// would say it.
func humanWait(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	m := int(d / time.Minute)
	if s := int((d % time.Minute).Seconds()); s != 0 {
		return fmt.Sprintf("%d 分 %d 秒", m, s)
	}
	return fmt.Sprintf("%d 分钟", m)
}

// PairingCard renders an unknown controller. Decision buttons are included
// only when the device's separate pairing-from-card switch is enabled.
func PairingCard(device string, pairing console.PendingPairing, nonce, portalURL string, withActions bool) map[string]any {
	elements := []any{
		map[string]any{
			"tag": "markdown",
			"content": fmt.Sprintf(
				"**控制器指纹**\n`%s`\n\n**名称**  %s\n\n**声明**  %s",
				pairing.FP, pairing.Name, pairing.Label,
			),
		},
		map[string]any{"tag": "hr"},
	}
	if withActions {
		elements = append(elements, decisionColumns("信任", "拒绝", nonce, "y", "n"))
	}
	elements = append(elements, portalButton(portalURL))
	return cardShell(
		"wanctl 控制器配对："+device,
		"控制器配对 · "+device,
		"未知控制器正在请求建立持久信任",
		"orange",
		"warning_colorful",
		"pairing",
		elements,
	)
}

// ResolvedPairingCard renders the terminal state of a controller pairing. It
// contains the same identity material as the request card and no actions.
func ResolvedPairingCard(device string, pairing console.PendingPairing, result, actor string) map[string]any {
	elements := []any{
		map[string]any{
			"tag": "markdown",
			"content": fmt.Sprintf(
				"**控制器指纹**\n`%s`\n\n**名称**  %s\n\n**声明**  %s",
				pairing.FP, pairing.Name, pairing.Label,
			),
		},
		map[string]any{"tag": "hr"},
		map[string]any{
			"tag":       "markdown",
			"content":   fmt.Sprintf("**结果**  %s<br>**决策人**  %s", result, actor),
			"text_size": "notation",
		},
	}
	return cardShell(
		"wanctl 配对结果："+device+" · "+result,
		"配对结果 · "+device,
		result,
		"grey",
		"",
		"pairing",
		elements,
	)
}

// ActionFailedCard replaces a card whose callback cannot be authorized. It is
// intentionally context-free because an unknown nonce (for example after a
// portal restart) has no trustworthy request data to render.
func ActionFailedCard(result string) map[string]any {
	return cardShell(
		"wanctl 卡片操作失败："+result,
		"卡片操作失败",
		result,
		"red",
		"error_colorful",
		"失效",
		[]any{map[string]any{
			"tag": "markdown", "content": "**结果**  " + result + "\n\n请返回门户查看当前设备状态。",
		}},
	)
}

// ResolvedCard renders the terminal state of a command or file approval. It
// deliberately contains no callback or URL buttons.
func ResolvedCard(device string, pending console.Pending, result, actor string) map[string]any {
	elements := pendingElements(pending)
	elements = append(elements,
		map[string]any{"tag": "hr"},
		map[string]any{
			"tag":       "markdown",
			"content":   fmt.Sprintf("**结果**  %s<br>**决策人**  %s", result, actor),
			"text_size": "notation",
		},
	)
	return cardShell(
		"wanctl 审批结果："+device+" · "+result,
		"审批结果 · "+device,
		result,
		"grey",
		"",
		pending.Kind,
		elements,
	)
}

func pendingElements(pending console.Pending) []any {
	elements := make([]any, 0, 2)
	if pending.Cmd != "" {
		elements = append(elements, map[string]any{
			"tag": "markdown", "content": "**命令**\n`" + eventlog.RedactText(pending.Cmd) + "`",
		})
	}
	if pending.Path != "" {
		elements = append(elements, map[string]any{
			"tag": "markdown", "content": "**路径**\n`" + eventlog.RedactText(pending.Path) + "`",
		})
	}
	context := fmt.Sprintf(
		"<font color='grey'>**发起方**  %s<br>**工作目录**  %s</font>",
		pending.Peer, eventlog.RedactText(pending.Cwd),
	)
	elements = append(elements, map[string]any{
		"tag": "markdown", "content": context, "text_size": "notation",
	})
	return elements
}

func decisionColumns(allowText, denyText, nonce, allowAction, denyAction string) map[string]any {
	return map[string]any{
		"tag":                "column_set",
		"horizontal_spacing": "medium",
		"columns": []any{
			map[string]any{
				"tag": "column", "width": "weighted", "weight": 1,
				"elements": []any{callbackButton(allowText, "primary_filled", nonce, allowAction)},
			},
			map[string]any{
				"tag": "column", "width": "weighted", "weight": 1,
				"elements": []any{callbackButton(denyText, "danger", nonce, denyAction)},
			},
		},
	}
}

func callbackButton(text, buttonType, nonce, action string) map[string]any {
	return map[string]any{
		"tag":   "button",
		"text":  map[string]any{"tag": "plain_text", "content": text},
		"type":  buttonType,
		"width": "fill",
		"behaviors": []any{map[string]any{
			"type":  "callback",
			"value": map[string]any{"a": action, "n": nonce},
		}},
	}
}

func portalButton(portalURL string) map[string]any {
	return map[string]any{
		"tag":       "button",
		"text":      map[string]any{"tag": "plain_text", "content": "在门户查看完整上下文"},
		"type":      "text",
		"size":      "small",
		"behaviors": []any{map[string]any{"type": "open_url", "default_url": portalURL}},
	}
}

func cardShell(summary, title, subtitle, template, icon, tag string, elements []any) map[string]any {
	header := map[string]any{
		"title":    map[string]any{"tag": "plain_text", "content": title},
		"subtitle": map[string]any{"tag": "plain_text", "content": subtitle},
		"template": template,
		"text_tag_list": []any{map[string]any{
			"tag": "text_tag", "text": map[string]any{"tag": "plain_text", "content": tag}, "color": template,
		}},
	}
	if icon != "" {
		header["icon"] = map[string]any{"tag": "standard_icon", "token": icon}
	}
	return map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"update_multi":   true,
			"width_mode":     "default",
			"enable_forward": false,
			"summary":        map[string]any{"content": summary},
		},
		"header": header,
		"body": map[string]any{
			"direction": "vertical",
			"padding":   "12px 12px 16px 12px",
			"elements":  elements,
		},
	}
}
