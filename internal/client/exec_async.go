package client

import (
	"context"
	"fmt"
	"io"

	"wanctl/internal/protocol"
)

// ExecAsync starts command as a background job on target and returns the job id
// immediately, without waiting for the command to finish. Poll the job later
// with ExecPollTo. Useful for commands that outlast a per-request timeout.
func (c *Client) ExecAsync(ctx context.Context, target, command, cwd string) (string, error) {
	conn, err := c.connect(ctx, target)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindExecAsync, Command: command, Cwd: cwd}); err != nil {
		return "", err
	}
	reply, err := protocol.ReadMessage(conn)
	if err != nil {
		return "", err
	}
	switch reply.Kind {
	case protocol.KindOK:
		if reply.JobID == "" {
			return "", fmt.Errorf("device accepted job but returned no id")
		}
		return reply.JobID, nil
	case protocol.KindReject:
		return "", rejectError(reply)
	case protocol.KindError:
		return "", fmt.Errorf("remote error: %s", reply.Reason)
	default:
		return "", fmt.Errorf("unexpected reply: %s", reply.Kind)
	}
}

// ExecPollTo fetches a background job's output produced after offset, writing it
// to out, and returns the new total output length, whether the job is still
// running, and (once finished) its exit code. Pass the returned newOffset as the
// next call's offset to stream incrementally.
func (c *Client) ExecPollTo(ctx context.Context, target, jobID string, offset int64, out io.Writer) (newOffset int64, running bool, code int, err error) {
	conn, err := c.connect(ctx, target)
	if err != nil {
		return offset, false, -1, err
	}
	defer conn.Close()
	if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindExecPoll, JobID: jobID, Offset: offset}); err != nil {
		return offset, false, -1, err
	}
	for {
		ft, payload, rerr := protocol.ReadFrame(conn)
		if rerr != nil {
			return offset, false, -1, rerr
		}
		switch ft {
		case protocol.FrameStdout:
			out.Write(payload)
		case protocol.FrameJSON:
			m, perr := protocol.DecodeMessage(payload)
			if perr != nil {
				return offset, false, -1, perr
			}
			switch m.Kind {
			case protocol.KindExit:
				return m.Offset, m.Running, m.Code, nil
			case protocol.KindError:
				return offset, false, -1, fmt.Errorf("remote error: %s", m.Reason)
			case protocol.KindReject:
				return offset, false, -1, rejectError(m)
			}
		}
	}
}
