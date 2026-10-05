package catalog

// The nested action schema is shared by MCP registration and generated docs.
func desktopActionItems() map[string]any {
	props := map[string]any{}
	for _, name := range []string{"button", "text", "key", "title", "program", "cwd"} {
		props[name] = map[string]any{"type": "string"}
	}
	for _, name := range []string{"x", "y", "to_x", "to_y", "count", "delta", "ms", "pid", "timeout_ms"} {
		props[name] = map[string]any{"type": "integer"}
	}
	props["type"] = map[string]any{"type": "string", "enum": []string{"click", "drag", "scroll", "type", "key", "wait", "focus", "launch"}}
	props["args"] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	return map[string]any{"type": "object", "properties": props, "required": []string{"type"}, "additionalProperties": false}
}
