package catalog

// MCPOutputSchema describes structuredContent, not the text in content. Error
// results may instead carry isError and the existing diagnostic text. Keep the
// schemas here with the input contract; both MCP transports use this method.
func (c Command) MCPOutputSchema() map[string]any {
	switch c.MCPName {
	case "wanctl_workspace":
		return workspaceOutputSchema()
	case "wanctl_exec":
		return executionOutputSchema(objectOutput("Completed command outside a workspace.", map[string]any{
			"target":           outputField("string", "Target supplied by the caller; may be an alias."),
			"done":             map[string]any{"type": "boolean", "const": true},
			"code":             outputField("integer", "Remote command exit code; zero means success."),
			"stdout":           outputField("string", "Bounded text from the stdout channel; device execution may merge stderr here. May include truncation notices and device-copy location."),
			"stderr":           outputField("string", "Bounded separate stderr-channel text; may be empty when device execution merges streams."),
			"stdout_truncated": outputField("boolean", "Whether stdout was shortened for this response."),
			"stderr_truncated": outputField("boolean", "Whether stderr was shortened for this response."),
			"spill_path":       outputField("string", "Device output-copy path, only when the device reported one. Not a local path."),
			"spill_bytes":      outputCount("Total output bytes counted by a spill-aware device; absent when unknown."),
			"spill_kept":       outputCount("Bytes kept in the device copy; a smaller value than spill_bytes means the copy is incomplete."),
		}, "target", "done", "code", "stdout", "stderr", "stdout_truncated", "stderr_truncated"))
	case "wanctl_exec_async":
		return executionOutputSchema(objectOutput("Accepted background job outside a workspace. Poll to observe completion.", map[string]any{
			"target": outputField("string", "Pass this target with job_id when polling."),
			"job_id": outputField("string", "Background job identifier; acceptance does not imply completion."),
		}, "target", "job_id"))
	case "wanctl_exec_poll":
		return executionOutputSchema(objectOutput("Background job output outside a workspace.", map[string]any{
			"target":      outputField("string", "Target supplied by the caller."),
			"job_id":      outputField("string", "Job being polled; keep it unchanged across polls."),
			"state":       outputEnum("Job state, not workspace state.", "running", "done"),
			"done":        outputField("boolean", "Whether the command has finished; an empty output page alone does not mean completion."),
			"code":        outputField("integer", "Final exit code; present only when done is true."),
			"output":      outputField("string", "New bounded output display text; may contain a truncation notice."),
			"next_offset": outputCount("Next byte offset in the device's output. Advances over the entire fetched page, including bytes omitted from display."),
			"truncated":   outputField("boolean", "Whether this fetched page was shortened for display; does not describe device retention."),
		}, "target", "job_id", "state", "done", "output", "next_offset", "truncated"))
	case "wanctl_peers":
		return objectOutput("Devices visible to this credential and their controller-side trust state.", map[string]any{
			"namespace": outputField("string", "Owner namespace of the credential; older relays may omit it."),
			"devices": map[string]any{"type": []string{"array", "null"}, "items": outputField("string", "Device ID."),
				"description": "Online device IDs in this namespace; null also means no devices."},
			"aliases": map[string]any{"type": []string{"object", "null"}, "additionalProperties": map[string]any{"type": "string"},
				"description": "Device ID to display alias; empty or null when none are known."},
			"identity": map[string]any{"type": "object", "additionalProperties": outputEnum("Controller trust state.", "pinned", "unpinned"),
				"description": "Canonical namespace/device target to trust state; not a device access grant."},
			"shared": map[string]any{"type": "array", "description": "Shared devices, including offline devices. Omitted when empty.",
				"items": objectOutput("A device shared by another namespace.", map[string]any{
					"owner": outputField("string", "Owner namespace."), "device": outputField("string", "Device ID."),
					"label": outputField("string", "Display label."), "online": outputField("boolean", "Current relay visibility."),
					"target": outputField("string", "Exact owner/device target to pass to subsequent tools."),
				}, "owner", "device", "label", "online", "target")},
		}, "devices", "aliases", "identity")
	case "wanctl_read":
		return objectOutput("A UTF-8 text-file range. Hash and size describe the entire file, not just this range.", map[string]any{
			"path":        outputField("string", "Requested remote path; relative paths use the workspace root."),
			"content":     outputField("string", "Returned file text, without display headers."),
			"first_line":  outputCount("First returned line, 1-based; may be zero for an empty range."),
			"last_line":   outputCount("Last returned line; may be zero for an empty range."),
			"total_lines": outputCount("Total lines in the entire file."),
			"size_bytes":  outputCount("Entire file size in bytes."),
			"sha256":      outputField("string", "SHA-256 of the entire file; pass as expected_sha256 when editing."),
			"truncated":   outputField("boolean", "The byte cap shortened this range; inspect long_line before continuing."),
			"long_line":   outputCount("Oversized line number, or zero. Nonzero means paging cannot recover that line; use an explicit bounded command instead."),
			"next_offset": outputCount("Next 1-based line offset after byte-cap truncation. Present only when paging can continue; not a byte offset."),
		}, "path", "content", "first_line", "last_line", "total_lines", "size_bytes", "sha256", "truncated", "long_line")
	case "wanctl_write":
		return objectOutput("Completed atomic file write.", map[string]any{
			"path":       outputField("string", "Requested remote path; relative paths use the workspace root."),
			"created":    outputField("boolean", "True for a new file, false when an existing file was replaced."),
			"size_bytes": outputCount("Written file size in bytes."),
			"sha256":     outputField("string", "SHA-256 of the written file."),
		}, "path", "created", "size_bytes", "sha256")
	case "wanctl_edit":
		return objectOutput("Completed atomic text edit.", map[string]any{
			"path":       outputField("string", "Requested remote path; relative paths use the workspace root."),
			"replaced":   outputCount("Number of replaced occurrences across this edit or batch."),
			"size_bytes": outputCount("Resulting file size in bytes."),
			"sha256":     outputField("string", "SHA-256 of the resulting file; replaces the pre-edit hash."),
		}, "path", "replaced", "size_bytes", "sha256")
	}
	return nil
}

func workspaceOutputSchema() map[string]any {
	return objectOutput("Workspace lifecycle or command result. Inspect state for lifecycle operations and done for command completion.", map[string]any{
		"workspace":         outputField("string", "Exact workspace reference to retain and pass to subsequent calls; not a credential."),
		"id":                outputField("string", "Workspace ID; use workspace, not this ID alone, when routing calls."),
		"root":              outputField("string", "Absolute project root; independent of shell cwd."),
		"state":             outputEnum("Workspace state. A successful exit returns closed even though done is false.", "open", "closed", "invalid"),
		"request_id":        outputField("string", "Command whose result this is; use as job_id in workspace polling."),
		"request_state":     outputEnum("Command state, when a request_id was inspected.", "approving", "running", "done"),
		"active_request_id": outputField("string", "Currently active command; absent when idle."),
		"done":              outputField("boolean", "Whether the inspected command finished. Lifecycle replies with no request_id use false; this does not indicate lifecycle failure."),
		"code":              outputField("integer", "Command exit code, meaningful only when request_id is present and done is true. Otherwise a zero placeholder."),
		"error":             outputField("string", "Command failure detail when available; MCP isError also signals execution errors."),
		"output":            outputField("string", "This page of merged textual command output; omitted when empty."),
		"next_offset":       outputCount("Next byte offset to poll. Even with done=true, continue while next_offset < retained_bytes."),
		"output_bytes":      outputCount("Total produced bytes; may exceed retained_bytes."),
		"retained_bytes":    outputCount("Bytes retained for this command; not a promise of a full log elsewhere."),
		"truncated":         outputField("boolean", "Some produced output was not retained. No implicit full-output file exists."),
	}, "workspace", "id", "root", "state", "done", "code", "next_offset", "output_bytes", "retained_bytes", "truncated")
}

func executionOutputSchema(legacy map[string]any) map[string]any {
	return map[string]any{"type": "object", "description": "The workspace form is returned when workspace is supplied or bound; otherwise the ordinary device form is returned.",
		"anyOf": []any{workspaceOutputSchema(), legacy}}
}

func objectOutput(description string, properties map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "description": description, "properties": properties, "required": required}
}

func outputField(kind, description string) map[string]any {
	return map[string]any{"type": kind, "description": description}
}

func outputCount(description string) map[string]any {
	return map[string]any{"type": "integer", "minimum": 0, "description": description}
}

func outputEnum(description string, values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values, "description": description}
}
