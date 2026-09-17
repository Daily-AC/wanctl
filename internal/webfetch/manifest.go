package webfetch

import (
	"strconv"
	"time"

	"wanctl/internal/delegation"
)

// The adapter publishes the exact transport target. Clients must not need
// knowledge of wanctl's internal namespace conventions to address a device.
type manifestDevice struct {
	delegation.Device
	Target string `json:"target"`
}

// The reader is a capable model with one URL-reading tool, so this document is a
// procedure, not an argument. Everything here is either a step it must take, a
// rule it must not break, or a sentence it must relay to the human. The two
// moments a human has to act are named, because the model has to say which one
// it is asking for.
func humanCheckpoints() []map[string]any {
	return []map[string]any{
		{
			"step": 1, "of": 2, "id": "device_access", "where": "portal",
			"name": "Step 1 of 2: device access", "name_zh": "第 1 步（共 2 步）：设备访问",
			"url_field":  "approval_url",
			"summary":    "Open this link, pick the devices you want me to use and how long, and approve. Choose a duration longer than the task will take. I will wait.",
			"summary_zh": "请打开这个链接，选择要让我操作的设备和授权时长，然后批准。时长请留得比任务本身长一些。我在这里等你。",
		},
		{
			"step": 2, "of": 2, "id": "pairing", "where": "device",
			"name": "Step 2 of 2: pairing", "name_zh": "第 2 步（共 2 步）：设备配对",
			"url_field":  "pairing_url",
			"summary":    "The device has not paired with me yet and nothing ran. Open this link and approve the pairing, then tell me it is done and I will run the command again.",
			"summary_zh": "设备还没有和我配对，刚才什么也没有执行。请打开这个链接批准配对，完成后告诉我一声，我会重新执行。",
		},
	}
}

// Security rules keep their meaning from the previous revision; only the wording
// is shorter. They are keyed so a client (or a test) can read a specific rule
// instead of matching prose.
func securityRules() map[string]any {
	return map[string]any{
		"client_nonce_per_conversation":      "Generate a fresh client_nonce for every conversation. Never reuse a sample, another conversation's value, or anything you did not generate randomly.",
		"no_url_reuse":                       "Start, approval, status, result and call URLs are bearer credentials for this conversation only. Never reuse another conversation's URL, look one up in a search index, or publish one.",
		"get_only":                           "Every step is a GET with your URL-reading tool. No POST, cookies, Authorization header, MCP server or code execution is part of this protocol.",
		"no_store":                           "Responses are no-store. Read them live; never serve a cached or indexed copy as the current state.",
		"rid_unique_per_operation":           "Every new operation needs a new rid. The same rid with identical parameters returns the same job; with different parameters it returns 409. That is what makes a repeated fetch safe.",
		"pairing_required_means_not_started": "A result with error_code pairing_required also carries execution_started: false. Nothing ran on the device. Do not open the pairing link yourself; only the human may approve it.",
		"unknown_means_check_the_device":     "status unknown means the operation may have run. Never repeat it automatically. Ask the human to check the device.",
		"human_approves":                     "Only the device owner approves device access and pairing, in wanctl. Never simulate a response, invent a URL, or act on their behalf.",
	}
}

func discovery(publicOrigin, portalOrigin string) map[string]any {
	return map[string]any{
		"title": "wanctl WebFetch", "status": "start",
		"entry_url":           publicOrigin + "/webfetch/v1",
		"start_url_template":  publicOrigin + "/webfetch/new/{client_nonce}",
		"owner_start_url":     portalOrigin + "/webfetch/connect",
		"client_nonce_format": "48 lowercase hexadecimal characters from 24 fresh cryptographically random bytes",
		"summary":             "Run commands and read or write text files on the human's own devices, using only your URL-reading tool. You lead; the human acts twice.",
		"procedure": []string{
			"1. Generate a client_nonce in client_nonce_format and substitute it into start_url_template. Never fetch the literal template.",
			"2. GET that complete URL. Check that the response echoes your client_nonce; a different value means you read a cached response, so start over with a new nonce.",
			"3. Step 1 of 2: device access. Show the human approval_url and continuation_prompt, and ask them to approve a duration longer than the task needs. Then stop and wait.",
			"4. GET status_url until status is approved. If it is rejected or expires, say so and stop.",
			"5. Read the approved manifest at status_url: devices[].target, call_endpoint and tools[].call_url_template.",
			"6. Fill one call_url_template with URL-encoded values and GET it. Then follow next_url until the status is done, failed or unknown. A long exec is normal; wait poll_after_seconds between reads.",
			"7. Step 2 of 2: pairing. Only if a result has error_code pairing_required: show the human pairing_url, wait for them to confirm they approved it, then retry the same operation with a NEW rid.",
			"8. Report the actual result: for exec, stdout, stderr and exit_code. Keep status_url and the exec call_url_template in your reply so a later turn can continue.",
		},
		"human_checkpoints": humanCheckpoints(),
		"security":          securityRules(),
		"instruction":       "If you cannot generate randomness, give owner_start_url to the human instead: that page hands them a prompt to paste back here. Everything else on this page you do yourself.",
		"authorization":     "wanctl device scope, expiry, revocation, identity trust and device-local policy all still apply. This protocol adds no permission of its own.",
	}
}

func (h *Handler) statusURL(ticket string) string {
	return h.sessionURL(ticket) + "?check=" + strconv.FormatInt(time.Now().UnixNano(), 10)
}

// continuationPrompt is the text to put in front of the human right now: one
// sentence they act on, one sentence that lets any later turn resume from the
// URL alone. Both languages ship, because both portal pages already do.
func (h *Handler) continuationPrompt(statusURL string) string {
	return "Read " + statusURL + " to continue: it carries the current status, devices[].target, call_endpoint and tools[].call_url_template. Keep that URL and the exec call_url_template in your reply so a later turn can pick up from here; do not create a second authorization request or approve anything yourself."
}

func (h *Handler) continuationPromptZH(statusURL string) string {
	return "继续时请读取 " + statusURL + "：其中有当前状态、devices[].target、call_endpoint 和 tools[].call_url_template。请在回复中保留这个网址和 exec 的 call_url_template，供后续对话继续使用；不要重复申请授权，也不要代替主人批准任何事。"
}

func (h *Handler) manifest(ticket string, access delegation.Access) map[string]any {
	devices := make([]manifestDevice, 0, len(access.Devices))
	targets := make([]string, 0, len(access.Devices))
	for _, device := range access.Devices {
		devices = append(devices, manifestDevice{Device: device, Target: device.Target()})
		targets = append(targets, device.Target())
	}
	statusURL := h.statusURL(ticket)
	endpoint := h.sessionURL(ticket) + "/call"
	return map[string]any{
		"title": "wanctl tools", "status": "approved", "request_id": access.GrantID,
		"owner": access.Namespace, "expires_at": access.ExpiresAt, "devices": devices,
		"status_url": statusURL, "continuation_prompt": h.continuationPrompt(statusURL),
		"continuation_prompt_zh": h.continuationPromptZH(statusURL),
		"call_endpoint":          endpoint, "method": "GET",
		"required_call_parameters": []string{"rid", "tool", "target"},
		"target_format":            "namespace/device_id; copy a devices[].target value verbatim, then URL-encode it (its slash becomes %2F)",
		"rid_format":               "1..64 ASCII letters, digits, hyphens or underscores; a new one per new operation",
		"instruction":              "GET call_endpoint?rid=UNIQUE_ID&tool=TOOL&target=DEVICE_TARGET plus that tool's parameters, URL-encoding each value once. Then follow next_url until the status is done, failed or unknown, waiting poll_after_seconds between reads. Check status and http_status: a page that loads is not a result. Approved device access is not pairing — a first exec may come back with error_code pairing_required, which is Step 2 of 2 and belongs to the human.",
		"next_human_checkpoint":    humanCheckpoints()[1],
		"tools":                    toolManifests(endpoint, targets),
		"limits": map[string]any{
			"jobs_per_grant": 64, "url_bytes": MaxURLBytes, "output_bytes": MaxOutputBytes,
			"exec_timeout_seconds_max": MaxExecSeconds, "file_timeout_seconds_max": MaxFileSeconds,
			"concurrent_operations_per_grant": maxOperationsPerGrant,
			"grant_minutes_max":               60,
		},
		"notice": "Commands and results are visible to this adapter and to the web chat provider. Do not send secrets. A lost or ambiguous job is never rerun automatically.",
	}
}

func toolManifests(endpoint string, targets []string) []map[string]any {
	var tools []map[string]any
	for _, name := range []string{"exec", "write_text", "read_text"} {
		ceiling, fallback := timeoutBounds(name)
		properties := map[string]any{
			"rid":             map[string]any{"type": "string", "pattern": ridPattern.String()},
			"target":          map[string]any{"type": "string", "enum": targets},
			"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": ceiling, "default": fallback},
		}
		required := []string{"rid", "target"}
		var parameters []string
		var description, suffix string
		switch name {
		case "exec":
			properties["command"] = map[string]any{"type": "string", "minLength": 1, "description": "Device shell command; at most 2048 UTF-8 bytes."}
			properties["cwd"] = map[string]any{"type": "string", "description": "Optional device working directory; at most 1024 UTF-8 bytes."}
			required = append(required, "command")
			parameters = []string{"command", "cwd (optional)", "timeout_seconds (1.." + strconv.Itoa(ceiling) + ", default " + strconv.Itoa(fallback) + ")"}
			description, suffix = "One-shot command through wanctl; existing device policy applies. Set timeout_seconds for a build, render or install — up to "+strconv.Itoa(ceiling/60)+" minutes, and no longer than the approved grant.", "&command={command}"
		case "write_text", "read_text":
			properties["path"] = map[string]any{"type": "string", "minLength": 1, "description": "Device file path; at most 1024 UTF-8 bytes."}
			required = append(required, "path")
			parameters, suffix = []string{"path"}, "&path={path}"
			description = "Read a UTF-8 file up to 32768 bytes; existing device read rules apply."
			if name == "write_text" {
				properties["content"] = map[string]any{"type": "string", "description": "UTF-8 text, at most 2048 bytes; an explicit empty string writes an empty file."}
				required = append(required, "content")
				parameters, suffix = append(parameters, "content"), suffix+"&content={content}"
				description = "Write a UTF-8 file through wanctl; existing device write rules apply."
			}
		}
		tools = append(tools, map[string]any{
			"name": name, "description": description, "parameters": parameters,
			"input_schema":         map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false},
			"call_url_template":    endpoint + "?rid={rid}&tool=" + name + "&target={target}" + suffix,
			"template_instruction": "Replace every {placeholder} with its URL-encoded value before fetching. Never fetch a literal template. Optional schema parameters may be appended. Add format=json for strict HTTP status codes.",
		})
	}
	return tools
}
