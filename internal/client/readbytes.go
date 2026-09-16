package client

import (
	"bytes"
	"context"
	"fmt"

	"wanctl/internal/protocol"
	"wanctl/internal/wsconn"
)

// PullBytes reads a bounded remote file without opening any local server file.
// It uses the same device-side file-get policy as Pull.
func (c *Client) PullBytes(ctx context.Context, target, path string, maxBytes int64) ([]byte, error) {
	if maxBytes < 0 || maxBytes > protocol.MaxFileSize {
		return nil, fmt.Errorf("invalid read limit")
	}
	conn, err := c.connect(ctx, target)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	defer wsconn.CloseOnCancel(ctx, conn)()
	if err = protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindFileGet, Path: path}); err != nil {
		return nil, err
	}
	meta, err := protocol.ReadMessage(conn)
	if err != nil {
		return nil, err
	}
	if meta.Kind == protocol.KindReject {
		return nil, rejectError(meta)
	}
	if meta.Kind == protocol.KindError {
		return nil, fmt.Errorf("remote refused download: %s", meta.Reason)
	}
	if meta.Kind != protocol.KindFileMeta || meta.Size < 0 || meta.Size > maxBytes {
		return nil, fmt.Errorf("remote file exceeds read limit or returned invalid metadata")
	}
	var out bytes.Buffer
	for {
		kind, payload, err := protocol.ReadFrame(conn)
		if err != nil {
			return nil, err
		}
		switch kind {
		case protocol.FrameData:
			if int64(out.Len())+int64(len(payload)) > maxBytes || int64(out.Len())+int64(len(payload)) > meta.Size {
				return nil, fmt.Errorf("remote file exceeds announced size or read limit")
			}
			out.Write(payload)
		case protocol.FrameJSON:
			message, err := protocol.DecodeMessage(payload)
			if err != nil {
				return nil, err
			}
			if message.Kind == protocol.KindEOF {
				if int64(out.Len()) != meta.Size {
					return nil, fmt.Errorf("remote file size mismatch")
				}
				return out.Bytes(), nil
			}
			if message.Kind == protocol.KindError {
				return nil, fmt.Errorf("remote read failed: %s", message.Reason)
			}
			return nil, fmt.Errorf("unexpected file response")
		default:
			return nil, fmt.Errorf("unexpected file frame")
		}
	}
}
