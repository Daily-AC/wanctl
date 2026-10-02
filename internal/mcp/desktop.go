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
	res, err := c.Act(ctx, reqStr(req, "target", ""), protocol.DesktopRequest{ScreenshotID: reqStr(req, "screenshot_id", ""), Actions: actions})
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
