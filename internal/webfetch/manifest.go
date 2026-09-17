package webfetch

import (
	"strconv"
	"time"

	"wanctl/internal/delegation"
	"wanctl/internal/protocol"
)

// The two file tools are the ones a model has to be taught, because the habit
// they replace — cat and sed over exec — looks like it works. These read as
// instructions to follow rather than as feature descriptions: when to reach for
// the tool, what the reply means, and what to do about each way it can refuse.
var (
	readDescription = "Read a line range of a UTF-8 text file, natively on the device: no shell parses anything, so it behaves the same on Linux, macOS, Windows and Android. Use this, not exec with cat/head/sed/Get-Content, whenever you want to LOOK at a file. Without offset and limit you get lines 1-2000. The reply says exactly what you got — first_line, last_line, the file's total_lines and size_bytes, and sha256 OF THE WHOLE FILE: keep that sha and pass it to edit_text as expected_sha256, which is how you prove you are patching the text you read. Paging: while last_line is below total_lines, read on with offset=last_line+1 under a NEW rid. truncated=true means the response cap cut the range on a line boundary and next_offset names the line to resume at, so nothing is lost and nothing repeats. long_line names a single line too large to return whole — do not ask for that line again, read it with exec (sed/cut). A file that is not UTF-8 text is refused; do not retry it here."

	editDescription = "Replace an exact string inside a text file, in place, natively on the device. This is how you CHANGE a file that already exists: nothing you send is parsed by a shell, so $, quotes and newlines arrive literally; the rest of the file is preserved byte for byte, CRLF stays CRLF and the mode is kept; and the write is atomic, so a reader never sees half a file. Prefer it over write_text, which replaces the whole file and silently discards anything written since you last read it. Workflow: read_text the file, copy its sha256 into expected_sha256, and put enough surrounding text in old that it matches exactly once. Every refusal leaves the file untouched and carries execution_started: false, so fix the input and send the corrected edit under a NEW rid: 'not found' means your old text does not appear (read the file again rather than guessing at whitespace), 'occurs N times' means add context or pass all=true, and 'changed since it was read' carries the file's current sha256. old and new travel inside the URL, which is capped at " + strconv.Itoa(MaxURLBytes) + " bytes in total, so edit one span at a time rather than rewriting a whole file. An edit needs the device's WRITE permission, exactly like write_text."
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
		"client_nonce_per_conversation":         "Generate a fresh client_nonce for every conversation. Never reuse a sample, another conversation's value, or anything you did not generate randomly.",
		"no_url_reuse":                          "Start, approval, status, result and call URLs are bearer credentials for this conversation only. Never reuse another conversation's URL, look one up in a search index, or publish one.",
		"get_only":                              "Every step is a GET with your URL-reading tool. No POST, cookies, Authorization header, MCP server or code execution is part of this protocol.",
		"no_store":                              "Responses are no-store. Read them live; never serve a cached or indexed copy as the current state.",
		"rid_unique_per_operation":              "One rid means one operation. A new operation needs a new rid.",
		"retry_a_lost_response_on_the_same_url": "If a call's response never arrives, or a result read fails midway, fetch the IDENTICAL URL again with the SAME rid and the SAME arguments. That returns the job already recorded and can never run the operation twice. Changing any argument under a used rid returns 409.",
		"new_rid_only_when_nothing_ran":         "Use a new rid only after a result that says nothing ran: error_code pairing_required, adapter_busy or file_refused, each with execution_started: false. A file_refused read or edit changed nothing on the device, so the corrected operation is a new one and needs a new rid. Never after a failed result without that flag, and never after unknown.",
		"pairing_required_means_not_started":    "A result with error_code pairing_required also carries execution_started: false. Nothing ran on the device. Do not open the pairing link yourself; only the human may approve it.",
		"unknown_means_check_the_device":        "status unknown means the operation may have run. Never repeat it automatically. Ask the human to check the device.",
		"human_approves":                        "Only the device owner approves device access and pairing, in wanctl. Never simulate a response, invent a URL, or act on their behalf.",
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
	return "Read " + statusURL + " to continue: it carries the current status, devices[].target, call_endpoint, tools[].call_url_template and the security rules, including retrying a lost response on the identical URL under the same rid. Keep that URL and the exec call_url_template in your reply so a later turn can pick up from here; do not create a second authorization request or approve anything yourself."
}

func (h *Handler) continuationPromptZH(statusURL string) string {
	return "继续时请读取 " + statusURL + "：其中有当前状态、devices[].target、call_endpoint、tools[].call_url_template 和安全规则，包括「响应丢失时用同一个 rid 重新读取完全相同的网址」。请在回复中保留这个网址和 exec 的 call_url_template，供后续对话继续使用；不要重复申请授权，也不要代替主人批准任何事。"
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
		"instruction":              "GET call_endpoint?rid=UNIQUE_ID&tool=TOOL&target=DEVICE_TARGET plus that tool's parameters, URL-encoding each value once. Then follow next_url until the status is done, failed or unknown, waiting poll_after_seconds between reads. If a response is lost, fetch the identical URL again with the same rid and arguments — that returns the same job instead of running it twice; a new rid is correct only when a result says nothing ran. Check status and http_status: a page that loads is not a result. Approved device access is not pairing — a first exec may come back with error_code pairing_required, which is Step 2 of 2 and belongs to the human.",
		"next_human_checkpoint":    humanCheckpoints()[1],
		"security":                 securityRules(),
		"tools":                    toolManifests(endpoint, targets),
		"limits": map[string]any{
			"jobs_per_grant": 64, "url_bytes": MaxURLBytes, "output_bytes": MaxOutputBytes,
			"exec_timeout_seconds_max": MaxExecSeconds, "file_timeout_seconds_max": MaxFileSeconds,
			"read_lines_default": protocol.DefaultReadLines, "write_bytes_max": MaxWriteBytes,
			"concurrent_operations_per_grant": maxOperationsPerGrant,
			"grant_minutes_max":               60,
		},
		"notice": "Commands and results are visible to this adapter and to the web chat provider. Do not send secrets. A lost or ambiguous job is never rerun automatically.",
	}
}

func toolManifests(endpoint string, targets []string) []map[string]any {
	var tools []map[string]any
	for _, name := range []string{"exec", "read_text", "edit_text", "write_text"} {
		ceiling, fallback := timeoutBounds(name)
		properties := map[string]any{
			"rid":             map[string]any{"type": "string", "pattern": ridPattern.String()},
			"target":          map[string]any{"type": "string", "enum": targets},
			"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": ceiling, "default": fallback},
		}
		required := []string{"rid", "target"}
		var parameters []string
		var description, suffix string
		if name != "exec" {
			properties["path"] = map[string]any{"type": "string", "minLength": 1, "description": "Absolute device file path; at most 1024 UTF-8 bytes. ~ is not expanded."}
			required = append(required, "path")
			parameters, suffix = []string{"path"}, "&path={path}"
		}
		switch name {
		case "exec":
			properties["command"] = map[string]any{"type": "string", "minLength": 1, "description": "Device shell command; at most 2048 UTF-8 bytes."}
			properties["cwd"] = map[string]any{"type": "string", "description": "Optional device working directory; at most 1024 UTF-8 bytes."}
			required = append(required, "command")
			parameters = []string{"command", "cwd (optional)", "timeout_seconds (1.." + strconv.Itoa(ceiling) + ", default " + strconv.Itoa(fallback) + ")"}
			description, suffix = "One-shot command through wanctl; existing device policy applies. Set timeout_seconds for a build, render or install — up to "+strconv.Itoa(ceiling/60)+" minutes, and no longer than the approved grant. To look at a file or change part of one, use read_text and edit_text instead of cat, sed or echo here: they run natively on the device, so nothing you send is parsed by a shell.", "&command={command}"
		case "read_text":
			properties["offset"] = map[string]any{"type": "integer", "minimum": 1, "maximum": maxLineNumber, "default": 1, "description": "1-based first line to return."}
			properties["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": maxReadLines, "default": protocol.DefaultReadLines, "description": "Maximum number of lines to return. The " + strconv.Itoa(MaxOutputBytes) + "-byte response cap still applies."}
			parameters = append(parameters, "offset (optional, default 1)", "limit (optional, default "+strconv.Itoa(protocol.DefaultReadLines)+")")
			description = readDescription
		case "edit_text":
			properties["old"] = map[string]any{"type": "string", "minLength": 1, "description": "The exact text to find, copied from a read_text of this file."}
			properties["new"] = map[string]any{"type": "string", "description": "The text to put in its place. Send new= explicitly to delete old."}
			properties["all"] = map[string]any{"type": "boolean", "default": false, "description": `"true" replaces every occurrence instead of refusing when old appears more than once.`}
			properties["expected_sha256"] = map[string]any{"type": "string", "pattern": sha256Pattern.String(), "description": "The sha256 read_text reported for this file. When set, the edit is refused unless the file still hashes to it."}
			required = append(required, "old", "new")
			parameters = append(parameters, "old", "new", "all (optional)", "expected_sha256 (optional)")
			description, suffix = editDescription, suffix+"&old={old}&new={new}"
		case "write_text":
			properties["content"] = map[string]any{"type": "string", "description": "UTF-8 text, at most 2048 bytes; an explicit empty string writes an empty file."}
			required = append(required, "content")
			parameters, suffix = append(parameters, "content"), suffix+"&content={content}"
			description = "Create a file, or replace a short one whole; existing device write rules apply. To change a file that already exists, use edit_text instead: this tool rewrites the whole file and discards anything written to it since you last read it."
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
