package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	mcpapi "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// escapeDeviceText makes the control characters in text from a device visible
// instead of live. A device's command output, error messages and names are
// chosen by whoever controls that device, and what a tool returns is read by a
// model and shown by its host — often in a terminal, where ESC starts a
// sequence that can rewrite what is on screen, and where the C1 controls
// (U+0080–U+009F) do the same in one character.
//
// Newline, carriage return and tab are kept. Every other C0 control, DEL and
// every byte that is not valid UTF-8 becomes \xNN, and a C1 control \u00NN, the
// way Go quotes them; printable text, non-ASCII included, is untouched. The
// result has no control characters left, so escaping it again changes nothing.
// These are the rules of the terminal escaping the CLI uses for what a device
// sends, with carriage return kept as that one keeps it for streamed output.
func escapeDeviceText(s string) string {
	var out []byte // nil until the first character that needs escaping
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		escaped := ""
		switch {
		case r == utf8.RuneError && size <= 1:
			escaped = fmt.Sprintf(`\x%02x`, s[i])
		case r == '\n' || r == '\r' || r == '\t':
		case r < 0x20 || r == 0x7f:
			escaped = fmt.Sprintf(`\x%02x`, r)
		case r >= 0x80 && r <= 0x9f:
			escaped = fmt.Sprintf(`\u%04x`, r)
		}
		if escaped != "" && out == nil {
			out = append(make([]byte, 0, len(s)+len(escaped)), s[:i]...)
		}
		switch {
		case escaped != "":
			out = append(out, escaped...)
		case out != nil:
			out = append(out, s[i:i+size]...)
		}
		i += size
	}
	if out == nil {
		return s
	}
	return string(out)
}

// rawContentTool is the one tool whose successful result goes back as the
// device sent it. wanctl_read returns a file's text so that wanctl_edit can
// match it exactly, byte for byte; escaping it would make every file with a
// control character in it impossible to edit. Its description says so.
const rawContentTool = "wanctl_read"

// escapeDeviceOutput runs every string in a tool's result through
// escapeDeviceText: the text a model reads, and every key and value of the
// structured content. It is applied to every tool rather than at each place a
// device's words are copied into a result — output, error reasons, names,
// pairing prompts, job IDs — because missing one of those places would be
// silent, and a tool's own wording has no control characters for this to
// change.
func escapeDeviceOutput(tool string, next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
		res, err := next(ctx, req)
		if err != nil {
			if msg := escapeDeviceText(err.Error()); msg != err.Error() {
				err = errors.New(msg)
			}
			return res, err
		}
		if res == nil || (tool == rawContentTool && !res.IsError) {
			return res, nil
		}
		escapeResult(res)
		return res, nil
	}
}

func escapeResult(res *mcpapi.CallToolResult) {
	for i, content := range res.Content {
		switch c := content.(type) {
		case mcpapi.TextContent:
			c.Text = escapeDeviceText(c.Text)
			res.Content[i] = c
		case *mcpapi.TextContent:
			c.Text = escapeDeviceText(c.Text)
		}
	}
	if res.StructuredContent == nil {
		return
	}
	// Structured content is whatever Go value the tool built — maps, typed
	// slices, structs. Its JSON shape is what reaches the client, and what the
	// output schema is checked against, so that is the shape escaped here.
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if dec.Decode(&generic) != nil {
		return
	}
	res.StructuredContent = escapeJSONValue(generic)
}

func escapeJSONValue(v any) any {
	switch x := v.(type) {
	case string:
		return escapeDeviceText(x)
	case []any:
		for i, item := range x {
			x[i] = escapeJSONValue(item)
		}
		return x
	case map[string]any:
		// Keys too: a listing keyed by device name is keyed by the device.
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[escapeDeviceText(k)] = escapeJSONValue(item)
		}
		return out
	}
	return v
}
