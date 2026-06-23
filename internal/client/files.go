package client

import (
	"context"
	"fmt"
	"io"
	"os"

	"wanctl/internal/protocol"
)

const fileChunk = 64 << 10

// Push uploads a local file to remotePath on the target device.
func (c *Client) Push(ctx context.Context, target, local, remotePath string) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}

	conn, err := c.connect(ctx, target)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := protocol.WriteMessage(conn, protocol.Message{
		Kind: protocol.KindFilePut,
		Path: remotePath,
		Size: info.Size(),
		Mode: uint32(info.Mode().Perm()),
	}); err != nil {
		return err
	}
	ack, err := protocol.ReadMessage(conn)
	if err != nil {
		return err
	}
	if ack.Kind == protocol.KindError {
		return fmt.Errorf("remote refused upload: %s", ack.Reason)
	}
	if ack.Kind != protocol.KindOK {
		return fmt.Errorf("unexpected reply: %s", ack.Kind)
	}

	buf := make([]byte, fileChunk)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if err := protocol.WriteFrame(conn, protocol.FrameData, buf[:n]); err != nil {
				return err
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindEOF}); err != nil {
		return err
	}
	done, err := protocol.ReadMessage(conn)
	if err != nil {
		return err
	}
	if done.Kind == protocol.KindError {
		return fmt.Errorf("remote write failed: %s", done.Reason)
	}
	fmt.Fprintf(os.Stderr, "pushed %s -> %s (%d bytes)\n", local, remotePath, info.Size())
	return nil
}

// Pull downloads remotePath from the target device into local.
func (c *Client) Pull(ctx context.Context, target, remotePath, local string) error {
	conn, err := c.connect(ctx, target)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindFileGet, Path: remotePath}); err != nil {
		return err
	}
	meta, err := protocol.ReadMessage(conn)
	if err != nil {
		return err
	}
	if meta.Kind == protocol.KindError {
		return fmt.Errorf("remote refused download: %s", meta.Reason)
	}
	if meta.Kind != protocol.KindFileMeta {
		return fmt.Errorf("unexpected reply: %s", meta.Kind)
	}

	mode := os.FileMode(meta.Mode)
	if mode == 0 {
		mode = 0o644
	}
	f, err := os.OpenFile(local, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer f.Close()

	var got int64
	for {
		ft, payload, err := protocol.ReadFrame(conn)
		if err != nil {
			return err
		}
		switch ft {
		case protocol.FrameData:
			if _, werr := f.Write(payload); werr != nil {
				return werr
			}
			got += int64(len(payload))
		case protocol.FrameJSON:
			m, _ := protocol.DecodeMessage(payload)
			if m.Kind == protocol.KindEOF {
				fmt.Fprintf(os.Stderr, "pulled %s -> %s (%d bytes)\n", remotePath, local, got)
				return nil
			}
			if m.Kind == protocol.KindError {
				return fmt.Errorf("remote read failed: %s", m.Reason)
			}
		}
	}
}
