package catalog

// desktopActionItems is the schema of one legacy action. It names the action
// types and nothing else: the act description already spells out every type's
// fields, and the agent refuses an unknown field, so declaring each field again
// here cost every caller three hundred tokens to say what the prose says.
func desktopActionItems() map[string]any {
	return map[string]any{"type": "object", "required": []string{"type"}, "properties": map[string]any{
		"type": map[string]any{"type": "string", "enum": []string{"click", "drag", "scroll", "type", "key", "wait", "focus", "launch"}},
	}}
}
