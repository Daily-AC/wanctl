package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	mcpapi "github.com/mark3labs/mcp-go/mcp"
	"wanctl/internal/client"
	"wanctl/internal/opencu"
	"wanctl/internal/protocol"
)

func mcpScreenshot(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	sess := sessions.get(ctx)
	c, hint := sess.client()
	if hint != nil {
		return hint, nil
	}
	shot := protocol.DesktopRequest{ScreenshotID: reqStr(req, "screenshot_id", "")}
	if raw, ok := req.GetArguments()["region"]; ok {
		encoded, err := json.Marshal(raw)
		if err != nil {
			return mcpapi.NewToolResultError("region must be [x,y,width,height]"), nil
		}
		var values []float64
		if json.Unmarshal(encoded, &values) != nil || len(values) != 4 {
			return mcpapi.NewToolResultError("region must be [x,y,width,height]"), nil
		}
		for _, v := range values {
			if v != math.Trunc(v) || v < 0 || v > 100000 {
				return mcpapi.NewToolResultError("region needs four nonnegative integers"), nil
			}
		}
		shot.Region = &protocol.Rect{X: int(values[0]), Y: int(values[1]), Width: int(values[2]), Height: int(values[3])}
	}
	res, err := c.Screenshot(ctx, reqStr(req, "target", ""), reqStr(req, "via", ""), shot)
	return desktopToolResponse(sess, res, err), nil
}
func mcpAct(ctx context.Context, req mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
	sess := sessions.get(ctx)
	c, hint := sess.client()
	if hint != nil {
		return hint, nil
	}
	actions, err := desktopActions(req.GetArguments()["actions"])
	if err != nil {
		return mcpapi.NewToolResultError(err.Error()), nil
	}
	request := protocol.DesktopRequest{ScreenshotID: reqStr(req, "screenshot_id", ""), Actions: actions, Tool: reqStr(req, "tool", "")}
	request.Discover, _ = req.GetArguments()["discover"].(bool)
	if raw, ok := req.GetArguments()["arguments"]; ok {
		request.Arguments, err = json.Marshal(raw)
		if err != nil {
			return mcpapi.NewToolResultError("invalid arguments object"), nil
		}
	}
	target := reqStr(req, "target", "")
	res, err := c.Act(ctx, target, request)
	return desktopToolResponse(sess, res, err), nil
}
func desktopActions(raw any) ([]protocol.DesktopAction, error) {
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, errors.New("actions must be a JSON array")
	}
	// Do not echo malformed content: the field that failed may contain text.
	var actions []protocol.DesktopAction
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&actions); err != nil {
		return nil, errors.New("invalid desktop action array")
	}
	return actions, nil
}
func desktopToolResponse(sess sessionAPI, res *client.DesktopResponse, err error) *mcpapi.CallToolResult {
	if res == nil {
		var lost *client.DesktopStateUnknownError
		var unsupported *client.UnsupportedError
		if errors.As(err, &lost) || errors.As(err, &unsupported) {
			return mcpapi.NewToolResultError(err.Error())
		}
		return dialErrorResult(sess, err)
	}
	if res.Legacy {
		return screenshotResult(res.Image)
	}
	header := res.Result
	if header.OpenCU != nil && err == nil {
		return openCUToolResponse(header)
	}
	if err != nil {
		header.Status = "unknown"
		header.Error = err.Error()
	}
	encoded, _ := json.Marshal(header)
	var result *mcpapi.CallToolResult
	if len(res.Image) > 0 {
		// Device JPEG bytes are the coordinate contract: NEVER call fitImage.
		result = mcpapi.NewToolResultImage(string(encoded), base64.StdEncoding.EncodeToString(res.Image), "image/jpeg")
	} else {
		result = mcpapi.NewToolResultText(string(encoded))
	}
	result.IsError = header.Status != "completed"
	if header.Error == protocol.DesktopHumanInput {
		result.Content = append(result.Content, mcpapi.TextContent{Type: "text", Text: fmt.Sprintf("%s. Stop and ask your user; never retry automatically.", protocol.DesktopHumanInput)})
	}
	return result
}

// Embedding Content preserves its sealed marker while MarshalJSON relays even
// future peer content kinds/fields without losing them through SDK decoding.
type openCUContent struct {
	mcpapi.Content
	raw json.RawMessage
}

func (c openCUContent) MarshalJSON() ([]byte, error) { return c.raw, nil }

func openCUToolResponse(header protocol.DesktopResult) *mcpapi.CallToolResult {
	r := header.OpenCU
	if len(r.Result) > 0 {
		var raw struct {
			Content    []json.RawMessage          `json:"content"`
			IsError    bool                       `json:"isError"`
			Meta       map[string]json.RawMessage `json:"_meta"`
			Structured json.RawMessage            `json:"structuredContent"`
		}
		if json.Unmarshal(r.Result, &raw) != nil {
			return mcpapi.NewToolResultError("invalid OpenCU result; outcome unknown; observe again, never replay")
		}
		result := &mcpapi.CallToolResult{IsError: raw.IsError, Content: make([]mcpapi.Content, 0, len(raw.Content))}
		for _, item := range raw.Content {
			result.Content = append(result.Content, openCUContent{raw: item})
		}
		if raw.Meta != nil {
			meta := map[string]any{}
			for k, v := range raw.Meta {
				meta[k] = v
			}
			result.Meta = mcpapi.NewMetaFromMap(meta)
		}
		if len(raw.Structured) > 0 {
			result.StructuredContent = raw.Structured
		}
		if opencu.ErrorCode(r.Result) == "PERSON_ACTIVE" {
			result.Content = append(result.Content, mcpapi.TextContent{Type: "text", Text: "Stop and ask your user; never retry automatically."})
		}
		return result
	}
	if len(r.Discovery) > 0 {
		return mcpapi.NewToolResultText(string(r.Discovery))
	}
	raw, _ := json.Marshal(r)
	return mcpapi.NewToolResultError(string(raw))
}

// Only human-readable text is escaped. Keep every other item and opaque field
// as raw JSON, preserving images, metadata and large structured numbers.
func (c openCUContent) escapeText() openCUContent {
	var item map[string]json.RawMessage
	if json.Unmarshal(c.raw, &item) != nil {
		return c
	}
	var kind, text string
	if json.Unmarshal(item["type"], &kind) != nil || kind != "text" {
		return c
	}
	if json.Unmarshal(item["text"], &text) != nil {
		return c
	}
	escaped := escapeDeviceText(text)
	if escaped == text {
		return c
	}
	item["text"], _ = json.Marshal(escaped)
	c.raw, _ = json.Marshal(item)
	return c
}
