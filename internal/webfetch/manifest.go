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

func (h *Handler) statusURL(ticket string) string {
	return h.sessionURL(ticket) + "?check=" + strconv.FormatInt(time.Now().UnixNano(), 10)
}

func continuationPrompt(statusURL string) string {
	return "Use your URL-reading tool to GET " + statusURL + ". Read the current authorization status and devices[].target values. Continue only my requested task after approval; do not create another authorization request or change device permissions. If expired or revoked, stop and report it."
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
		"status_url": statusURL, "continuation_prompt": continuationPrompt(statusURL),
		"call_endpoint": endpoint, "method": "GET",
		"required_call_parameters": []string{"rid", "tool", "target"},
		"target_format":            "namespace/device_id; copy a devices[].target value verbatim, then URL-encode it",
		"rid_format":               "1..64 ASCII letters, digits, hyphens or underscores; unique per new operation",
		"instruction":              "Construct GET call_endpoint?rid=UNIQUE_ID&tool=TOOL&target=DEVICE_TARGET plus the tool parameters. Copy DEVICE_TARGET exactly from devices[].target; never guess a namespace, ID or separator. URL-encode each query value once (a target's slash becomes %2F). Reuse exactly the same rid and arguments for retries. Poll result_url/next_url until done; at most 8 polls. Check status and http_status even when the HTML page loads successfully. Report errors instead of enumerating argument formats. Approved device access does NOT imply pairing; the owner must perform any pairing or device-policy approval in wanctl, never the AI. For a later chat turn, give the owner continuation_prompt with its full URL.",
		"tools":                    toolManifests(endpoint, targets),
		"limits":                   map[string]any{"jobs_per_grant": 64, "url_bytes": MaxURLBytes, "output_bytes": MaxOutputBytes},
		"notice":                   "Commands and results are visible to this adapter and the web chat provider. Do not send secrets. A lost/ambiguous job is never automatically rerun.",
	}
}

func toolManifests(endpoint string, targets []string) []map[string]any {
	var tools []map[string]any
	for _, name := range []string{"exec", "write_text", "read_text"} {
		properties := map[string]any{
			"rid":             map[string]any{"type": "string", "pattern": ridPattern.String()},
			"target":          map[string]any{"type": "string", "enum": targets},
			"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 60, "default": 30},
		}
		required := []string{"rid", "target"}
		var parameters []string
		var description, suffix string
		switch name {
		case "exec":
			properties["command"] = map[string]any{"type": "string", "minLength": 1, "description": "Device shell command; at most 2048 UTF-8 bytes."}
			properties["cwd"] = map[string]any{"type": "string", "description": "Optional device working directory; at most 1024 UTF-8 bytes."}
			required = append(required, "command")
			parameters = []string{"command", "cwd (optional)", "timeout_seconds (1..60, default30)"}
			description, suffix = "One-shot command through wanctl; existing device policy applies.", "&command={command}"
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
			"template_instruction": "Replace every {placeholder} with its URL-encoded value before fetching. Do not fetch a literal template. Optional schema parameters may be appended. Add format=json for strict HTTP errors in programmatic clients.",
		})
	}
	return tools
}
