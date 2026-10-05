package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"net"
	"strings"

	"wanctl/internal/desktop"
	"wanctl/internal/protocol"
	"wanctl/internal/wsconn"
)

type DesktopResponse struct {
	Result protocol.DesktopResult
	Image  []byte
	Legacy bool // old screenshot: PNG without a coordinate ID
}

// DesktopStateUnknownError must never be handled with an automatic retry.
// Completed actions, if a header arrived, remain available in the response.
type DesktopStateUnknownError struct{ Cause error }

func (e *DesktopStateUnknownError) Error() string {
	return "state unknown: desktop input may be partially completed; do not replay; ask the user before acting again"
}
func (e *DesktopStateUnknownError) Unwrap() error { return e.Cause }

func (c *Client) Act(ctx context.Context, target string, req protocol.DesktopRequest) (*DesktopResponse, error) {
	return c.desktopCall(ctx, target, "act", "", req)
}
func (c *Client) Screenshot(ctx context.Context, target, via string, req protocol.DesktopRequest) (*DesktopResponse, error) {
	return c.desktopCall(ctx, target, "screenshot", via, req)
}
func (c *Client) desktopCall(ctx context.Context, target, action, via string, req protocol.DesktopRequest) (*DesktopResponse, error) {
	return desktopCallWithDial(ctx, target, action, via, req, func(ctx context.Context) (net.Conn, error) { return c.connect(ctx, target) })
}

func desktopCallWithDial(ctx context.Context, target, action, via string, req protocol.DesktopRequest, dial func(context.Context) (net.Conn, error)) (*DesktopResponse, error) {
	if err := desktop.Validate(action, &req); err != nil {
		return nil, err
	}
	conn, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	defer wsconn.CloseOnCancel(ctx, conn)()
	res, err := desktopOver(conn, protocol.Message{Kind: protocol.KindDesktop, Action: action, RequestID: desktop.NewID(), Desktop: &req})
	if action != "screenshot" {
		return res, err
	}
	if errors.Is(err, desktop.ErrWindowsOnly) {
		if req.Region != nil {
			return nil, errors.New("region screenshots require the new Windows desktop agent")
		}
		// New non-Windows agents leave the stream ready for the legacy verb.
		return legacyScreenshotOver(ctx, conn, target, via)
	}
	var unsupported *UnsupportedError
	if errors.As(err, &unsupported) {
		unsupported.Target = target
		if req.Region != nil {
			return nil, unsupported
		}
		// Old agents close after unknown request. Reconnect only for this known
		// read-only compatibility case, never after ambiguous transport failure.
		conn.Close()
		old, err := dial(ctx)
		if err != nil {
			return nil, err
		}
		defer old.Close()
		defer wsconn.CloseOnCancel(ctx, old)()
		return legacyScreenshotOver(ctx, old, target, via)
	}
	return res, err
}
func desktopOver(rw io.ReadWriter, req protocol.Message) (*DesktopResponse, error) {
	unknown := func(err error) error {
		if req.Action == "act" {
			return &DesktopStateUnknownError{Cause: err}
		}
		return err
	}
	if err := protocol.WriteMessage(rw, req); err != nil {
		return nil, unknown(err)
	}
	m, err := protocol.ReadMessage(rw)
	if err != nil {
		return nil, unknown(err)
	}
	switch m.Kind {
	case protocol.KindReject:
		return nil, rejectError(m)
	case protocol.KindError:
		if strings.HasPrefix(m.Reason, "unknown request") {
			return nil, &UnsupportedError{Kind: protocol.KindDesktop}
		}
		if m.Reason == protocol.DesktopWindowsOnly {
			return nil, desktop.ErrWindowsOnly
		}
		return nil, errors.New(m.Reason)
	}
	header, data, err := desktop.ReadResultBody(rw, m)
	res := &DesktopResponse{Result: header, Image: data}
	if err != nil {
		return res, unknown(err)
	}
	if header.RequestID != req.RequestID {
		return res, unknown(errors.New("desktop request/result identity mismatch"))
	}
	if len(data) > 0 {
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
		if err != nil || header.Snapshot == nil || header.Snapshot.Width != cfg.Width || header.Snapshot.Height != cfg.Height || max(cfg.Width, cfg.Height) > 1280 {
			return res, unknown(errors.New("desktop image dimensions do not match its coordinate reference"))
		}
	}
	return res, nil
}
func legacyScreenshotOver(ctx context.Context, rw io.ReadWriter, target, via string) (*DesktopResponse, error) {
	req := ExecRequest{Target: target, Command: "screenshot", OneShot: true, Elevate: true, ElevateOptional: true, Via: via}
	if err := protocol.WriteMessage(rw, protocol.Message{Kind: protocol.KindExec, Command: req.Command, OneShot: true, Elevate: true, Via: via}); err != nil {
		return nil, err
	}
	var image, stderr bytes.Buffer
	outcome, err := execOver(ctx, rw, req, &image, &stderr)
	if err != nil {
		return nil, err
	}
	if outcome.Code != 0 {
		return nil, fmt.Errorf("screenshot failed (exit %d): %s", outcome.Code, strings.TrimSpace(stderr.String()))
	}
	if !bytes.HasPrefix(image.Bytes(), []byte("\x89PNG\r\n\x1a\n")) {
		return nil, errors.New("device did not return a PNG; update its agent")
	}
	return &DesktopResponse{Result: protocol.DesktopResult{Status: "completed", FailedIndex: -1, ImageBytes: image.Len(), MIME: "image/png"}, Image: image.Bytes(), Legacy: true}, nil
}
