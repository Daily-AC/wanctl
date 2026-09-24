package client

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"

	"wanctl/internal/protocol"
	"wanctl/internal/wsconn"
)

const fileChunk = 64 << 10

// pipelinedPutBytes is the largest upload sent without waiting for the
// device's go-ahead. A small file costs less to send than the round trip that
// waiting for the acknowledgement takes, and a device that refuses it has not
// written a byte: the gate answers before any data frame is read, and every
// agent version stops reading at the refusal. A larger file still waits, so a
// refusal does not cost a whole upload.
const pipelinedPutBytes = fileChunk

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
	usedDirect, err := c.pushReaderPath(ctx, target, remotePath, f, info.Size(), uint32(info.Mode().Perm()))
	if err != nil {
		return err
	}
	if usedDirect {
		fmt.Fprintf(os.Stderr, "pushed %s -> %s (%d bytes, direct)\n", local, remotePath, info.Size())
	} else {
		fmt.Fprintf(os.Stderr, "pushed %s -> %s (%d bytes)\n", local, remotePath, info.Size())
	}
	return nil
}

// PushBytes uploads in-memory content to remotePath on the target device — the
// transport for HTTP/remote MCP mode, where the AI host has no file on the MCP
// server to point Push at (issue #6). mode 0 falls back to 0644.
func (c *Client) PushBytes(ctx context.Context, target, remotePath string, data []byte, mode uint32) error {
	if mode == 0 {
		mode = 0o644
	}
	return c.pushReader(ctx, target, remotePath, bytes.NewReader(data), int64(len(data)), mode)
}

// pushReader streams size bytes from r to remotePath on target, honoring the
// device's file-put policy gate. Shared by Push (local file) and PushBytes
// (in-memory blob).
func (c *Client) pushReader(ctx context.Context, target, remotePath string, r io.Reader, size int64, mode uint32) error {
	_, err := c.pushReaderPath(ctx, target, remotePath, r, size, mode)
	return err
}

func (c *Client) pushReaderPath(ctx context.Context, target, remotePath string, r io.Reader, size int64, mode uint32) (bool, error) {
	if size < 0 || size > protocol.MaxFileSize {
		return false, fmt.Errorf("upload size %d outside supported range 0..%d", size, protocol.MaxFileSize)
	}
	conn, err := c.connectPipelined(ctx, target)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	defer wsconn.CloseOnCancel(ctx, conn)()

	request := protocol.Message{
		Kind: protocol.KindFilePut,
		Path: remotePath,
		Size: size,
		Mode: mode,
	}
	if c.directEnabled && size >= c.directSettings.Effective().MinBytes {
		request.Direct = &protocol.DirectInfo{}
	}
	if err := protocol.WriteMessage(conn, request); err != nil {
		return false, err
	}
	readAck := func() (protocol.Message, error) {
		ack, err := protocol.ReadMessage(conn)
		if err != nil {
			return protocol.Message{}, err
		}
		if ack.Kind == protocol.KindError {
			return protocol.Message{}, fmt.Errorf("remote refused upload: %s", ack.Reason)
		}
		if ack.Kind == protocol.KindReject {
			return protocol.Message{}, rejectError(ack)
		}
		if ack.Kind != protocol.KindOK {
			return protocol.Message{}, fmt.Errorf("unexpected reply: %s", ack.Kind)
		}
		return ack, nil
	}
	pipelined := size <= pipelinedPutBytes && request.Direct == nil
	path := relayPath(conn)
	if !pipelined {
		ack, err := readAck()
		if err != nil {
			return false, err
		}
		if ack.Direct != nil {
			path, err = c.selectFilePath(ctx, conn, ack.Direct)
			if err != nil {
				return false, err
			}
		}
	}
	defer path.close()
	rw := path.rw
	wrap := func(err error, unknown bool) (bool, error) {
		if !path.direct {
			return false, err
		}
		if unknown {
			return true, fmt.Errorf("direct push result unknown; device may have committed: %w", err)
		}
		return true, fmt.Errorf("direct push failed: %w", err)
	}

	buf := make([]byte, fileChunk)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if err := protocol.WriteFrame(rw, protocol.FrameData, buf[:n]); err != nil {
				return wrap(err, false)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return wrap(rerr, false)
		}
	}
	if err := protocol.WriteMessage(rw, protocol.Message{Kind: protocol.KindEOF}); err != nil {
		return wrap(err, false)
	}
	if pipelined {
		if _, err := readAck(); err != nil {
			return false, err
		}
	}
	done, err := protocol.ReadMessage(rw)
	if err != nil {
		return wrap(err, true)
	}
	if done.Kind == protocol.KindError {
		return wrap(fmt.Errorf("remote write failed: %s", done.Reason), false)
	}
	if done.Kind != protocol.KindOK {
		return wrap(fmt.Errorf("unexpected upload result: %s", done.Kind), true)
	}
	if done.Size != size {
		return wrap(fmt.Errorf("remote write size mismatch: got %d, want %d", done.Size, size), true)
	}
	return path.direct, nil
}

// Pull downloads remotePath from the target device into local.
func (c *Client) Pull(ctx context.Context, target, remotePath, local string) error {
	conn, err := c.connectPipelined(ctx, target)
	if err != nil {
		return err
	}
	defer conn.Close()
	defer wsconn.CloseOnCancel(ctx, conn)()

	request := protocol.Message{Kind: protocol.KindFileGet, Path: remotePath}
	if c.directEnabled {
		request.Direct = &protocol.DirectInfo{}
	}
	if err := protocol.WriteMessage(conn, request); err != nil {
		return err
	}
	meta, err := protocol.ReadMessage(conn)
	if err != nil {
		return err
	}
	if meta.Kind == protocol.KindError {
		return fmt.Errorf("remote refused download: %s", meta.Reason)
	}
	if meta.Kind == protocol.KindReject {
		return rejectError(meta)
	}
	if meta.Kind != protocol.KindFileMeta {
		return fmt.Errorf("unexpected reply: %s", meta.Kind)
	}
	path := relayPath(conn)
	if meta.Direct != nil {
		path, err = c.selectFilePath(ctx, conn, meta.Direct)
		if err != nil {
			return err
		}
	}
	defer path.close()

	mode := os.FileMode(meta.Mode)
	if mode == 0 {
		mode = 0o644
	}
	f, err := os.OpenFile(local, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer f.Close()

	got, err := receiveFile(path.rw, f, meta.Size, local, func(n int64) error {
		if c.pullProgress != nil {
			c.pullProgress(n)
		}
		return ctx.Err()
	})
	if err != nil {
		if path.direct {
			return fmt.Errorf("direct pull failed: %w", err)
		}
		return err
	}
	if path.direct {
		fmt.Fprintf(os.Stderr, "pulled %s -> %s (%d bytes, direct)\n", remotePath, local, got)
	} else {
		fmt.Fprintf(os.Stderr, "pulled %s -> %s (%d bytes)\n", remotePath, local, got)
	}
	return nil
}

func receiveFile(src io.Reader, dst io.Writer, expected int64, local string, progress ...func(int64) error) (int64, error) {
	var got int64
	incomplete := func(err error) (int64, error) { return got, fmt.Errorf("local file %s is incomplete: %w", local, err) }
	for {
		ft, payload, err := protocol.ReadFrame(src)
		if err != nil {
			return incomplete(err)
		}
		switch ft {
		case protocol.FrameData:
			n, werr := dst.Write(payload)
			got += int64(n)
			if werr != nil {
				return incomplete(werr)
			}
			if n != len(payload) {
				return incomplete(io.ErrShortWrite)
			}
			if got > expected {
				return incomplete(fmt.Errorf("received %d bytes, expected %d", got, expected))
			}
			if len(progress) > 0 && progress[0] != nil {
				if err := progress[0](got); err != nil {
					return incomplete(err)
				}
			}
		case protocol.FrameJSON:
			m, err := protocol.DecodeMessage(payload)
			if err != nil {
				return incomplete(err)
			}
			if m.Kind == protocol.KindEOF {
				if got != expected {
					return incomplete(fmt.Errorf("received %d bytes, expected %d", got, expected))
				}
				return got, nil
			}
			if m.Kind == protocol.KindError {
				return incomplete(fmt.Errorf("remote read failed: %s", m.Reason))
			}
			return incomplete(fmt.Errorf("unexpected download control: %s", m.Kind))
		default:
			return incomplete(fmt.Errorf("unexpected download frame: %d", ft))
		}
	}
}
