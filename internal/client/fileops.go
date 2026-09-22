package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"wanctl/internal/protocol"
	"wanctl/internal/wsconn"
)

// ReadRequest asks a device for a line range of one text file.
type ReadRequest struct {
	WorkspaceID string
	Target      string
	Path        string // absolute, or relative to WorkspaceID on the device
	Offset      int    // 1-based first line; 0 means line 1
	Limit       int    // max lines; 0 means protocol.DefaultReadLines
}

// EditRequest asks a device to replace text inside one file. It takes either a
// single Old/New pair or a batch of Edits, never both.
type EditRequest struct {
	WorkspaceID string
	Target      string
	Path        string // absolute, or relative to WorkspaceID on the device
	Old         string // non-empty, unless Edits is used instead
	New         string // may be empty, which deletes Old
	All         bool   // replace every occurrence instead of refusing on more than one
	ExpectedSHA string // optional: refuse unless the file still hashes to this
	// Edits is the batch form: several replacements, each matching the file as
	// it was before any of them ran, applied in one atomic rewrite or refused
	// together.
	Edits []protocol.FileEdit
}

// WriteRequest asks a device to create or completely replace one text file.
type WriteRequest struct {
	WorkspaceID string
	Target      string
	Path        string // absolute, or relative to WorkspaceID on the device; missing parent directories are created
	Content     string // the whole new text of the file, UTF-8
}

// WriteResult is what a device reports after a write.
type WriteResult struct {
	Created   bool // the file did not exist before
	SizeBytes int64
	SHA256    string
}

// ReadResult is what a device reports for a read.
type ReadResult struct {
	Content    string
	TotalLines int
	FirstLine  int
	LastLine   int
	SizeBytes  int64
	SHA256     string
	Truncated  bool
	// LongLine names a line too large to return whole, whose first 256 KiB came
	// back instead. Paging past it is the one continuation that cannot work.
	LongLine int
}

// EditResult is what a device reports for an applied edit.
type EditResult struct {
	Replaced  int
	SizeBytes int64
	SHA256    string
}

// UnsupportedError says the device is running an agent from before file_read /
// file_edit existed. It is a distinct type because the fix is a specific one —
// update the agent — and every surface should be able to say so in its own
// words rather than passing on "unknown request: file_read".
type UnsupportedError struct {
	Target  string
	Version string // the device's agent version, when it could be asked
	Kind    string // the frame kind it rejected
}

func (e *UnsupportedError) Error() string {
	agent := "device agent"
	if e.Version != "" {
		agent = "device agent " + e.Version
	}
	return fmt.Sprintf("%s does not support %s; run `wanctl update` on the device", agent, unsupportedWhat(e.Kind))
}

// unsupportedWhat names the missing capability the way a caller would ask for
// it, so the sentence is about what they tried to do rather than about a frame
// kind they never chose.
func unsupportedWhat(kind string) string {
	switch kind {
	case protocol.KindWorkspace:
		return "workspaces"
	case protocol.KindFileWrite:
		return "write"
	default:
		return "read/edit"
	}
}

// ResultLostError says the request reached the device and the answer did not
// come back: the connection ended after the frame was sent.
//
// It is deliberately not an error about the operation failing, because nobody
// knows whether it failed. For an edit or a write that distinction is the whole
// message — the file may already hold the new text — so the instruction is to
// look before acting rather than to retry, which for a non-idempotent edit
// would be a second replacement of text that is no longer there.
type ResultLostError struct {
	Target string
	Kind   string // the frame kind whose result was lost
	Path   string // the file it named
	Cause  error  // what ended the exchange, kept so the diagnosis is not lost
}

func (e *ResultLostError) Unwrap() error { return e.Cause }

func (e *ResultLostError) Error() string {
	where := ""
	if e.Path != "" {
		where = " " + e.Path
	}
	because := ""
	if e.Cause != nil && !errors.Is(e.Cause, io.EOF) && !errors.Is(e.Cause, io.ErrUnexpectedEOF) {
		// A reset or a timeout is worth naming: it is the difference between a
		// device that went away and a network that is failing under the caller.
		because = " (" + e.Cause.Error() + ")"
	}
	if e.Kind == protocol.KindFileRead {
		// A read changes nothing, so there is nothing to inspect and retrying
		// is free. Saying so keeps the caller from an anxious hash check it
		// does not need.
		return fmt.Sprintf("result unknown: the connection dropped after the request was sent%s; nothing was changed by reading%s, so retry", because, where)
	}
	return fmt.Sprintf(
		"result unknown: the connection dropped after the request was sent%s; read the file and compare sha256 before retrying — the change to%s may or may not have been applied",
		because, where)
}

// ReadFile returns a line range of a file on the target device.
func (c *Client) ReadFile(ctx context.Context, req ReadRequest) (*ReadResult, error) {
	res, err := c.fileOp(ctx, req.Target, protocol.Message{
		WorkspaceID: req.WorkspaceID,
		Kind:        protocol.KindFileRead,
		Path:        req.Path,
		Offset:      int64(req.Offset),
		Limit:       req.Limit,
	})
	if err != nil {
		return nil, err
	}
	return &ReadResult{
		Content:    res.Content,
		TotalLines: res.TotalLines,
		FirstLine:  res.FirstLine,
		LastLine:   res.LastLine,
		SizeBytes:  res.SizeBytes,
		SHA256:     res.SHA256,
		Truncated:  res.Truncated,
		LongLine:   res.LongLine,
	}, nil
}

// EditFile replaces a string inside a file on the target device. A refusal —
// the string was not found, it was found more than once, the file changed since
// it was read — comes back as an error and leaves the file untouched.
func (c *Client) EditFile(ctx context.Context, req EditRequest) (*EditResult, error) {
	switch {
	case len(req.Edits) > 0 && (req.Old != "" || req.New != ""):
		return nil, errors.New("pass either 'old'/'new' or 'edits', not both")
	case len(req.Edits) == 0 && req.Old == "":
		return nil, errors.New("edit needs a non-empty 'old' string to find, or an 'edits' array")
	}
	res, err := c.fileOp(ctx, req.Target, protocol.Message{
		Kind:        protocol.KindFileEdit,
		WorkspaceID: req.WorkspaceID,
		Path:        req.Path,
		Old:         req.Old,
		New:         req.New,
		All:         req.All,
		ExpectedSHA: req.ExpectedSHA,
		Edits:       req.Edits,
	})
	if err != nil {
		return nil, err
	}
	return &EditResult{Replaced: res.Replaced, SizeBytes: res.SizeBytes, SHA256: res.SHA256}, nil
}

// WriteFile creates or completely replaces a text file on the target device,
// making any missing parent directories. It is the whole-file counterpart of
// EditFile: reach for it when the file is new or is being rewritten end to end,
// and for anything else edit the part that changes.
func (c *Client) WriteFile(ctx context.Context, req WriteRequest) (*WriteResult, error) {
	res, err := c.fileOp(ctx, req.Target, protocol.Message{
		WorkspaceID: req.WorkspaceID,
		Kind:        protocol.KindFileWrite,
		Path:        req.Path,
		Content:     req.Content,
	})
	if err != nil {
		return nil, err
	}
	return &WriteResult{Created: res.Created, SizeBytes: res.SizeBytes, SHA256: res.SHA256}, nil
}

// fileOp dials the target, sends one file_read/file_edit request and returns the
// device's result.
func (c *Client) fileOp(ctx context.Context, target string, req protocol.Message) (*protocol.FileResult, error) {
	if req.WorkspaceID == "" && !strings.HasPrefix(req.Path, "/") && !hasWindowsDrive(req.Path) {
		return nil, fmt.Errorf("path %q must be absolute on the device (~ is not expanded)", req.Path)
	}
	var res *protocol.FileResult
	var err error
	if req.WorkspaceID != "" && c.workspaceLink != nil {
		req.Kind, req.Action = protocol.KindWorkspace, req.Kind
		var reply protocol.Message
		reply, err = c.workspaceRoundTrip(ctx, WorkspaceRef{Target: target, ID: req.WorkspaceID}, req)
		if err == nil {
			res, err = fileReply(req, reply)
		} else {
			var lost *workspaceExchangeError
			if errors.As(err, &lost) {
				err = &ResultLostError{Kind: req.Action, Path: req.Path, Cause: err}
			}
		}
	} else {
		conn, e := c.connect(ctx, target)
		if e != nil {
			return nil, e
		}
		defer conn.Close()
		defer wsconn.CloseOnCancel(ctx, conn)()
		if req.WorkspaceID != "" {
			req.Kind, req.Action = protocol.KindWorkspace, req.Kind
		}
		res, err = fileOpOver(conn, req)
	}

	var lost *ResultLostError
	if errors.As(err, &lost) {
		lost.Target = target
	}
	var unsupported *UnsupportedError
	if errors.As(err, &unsupported) {
		unsupported.Target = target
		// The device rejected the frame and closed the session, so ask a
		// second, older verb which agents have understood for far longer what
		// version it is. Best effort: the message reads fine without it.
		unsupported.Version = c.agentVersion(ctx, target)
	}
	return res, err
}

// fileOpOver performs the request/response exchange on an already-open session.
// It is separate from the dial so the wire behaviour can be tested against a
// stand-in device, including one that predates these frame kinds.
func fileOpOver(rw io.ReadWriter, req protocol.Message) (*protocol.FileResult, error) {
	if err := protocol.WriteMessage(rw, req); err != nil {
		return nil, err
	}
	reply, err := protocol.ReadMessage(rw)
	if err != nil {
		// Anything that goes wrong AFTER the request frame was sent says
		// nothing about whether the device ran it — a clean EOF, a reset, a
		// timeout, a half-read frame. This used to read an EOF as "an agent too
		// old to have a default branch dropped the session", which is one thing
		// it can be; telling a caller whose edit HAD been applied that nothing
		// ran and to update the agent is the worst answer available, because
		// the fix it names is useless and the claim it makes is false. Only an
		// explicit `unknown request` reply proves the device did not run this,
		// and every other ending is an unknown result carrying its own cause.
		kind := req.Kind
		if kind == protocol.KindWorkspace {
			kind = req.Action
		}
		return nil, &ResultLostError{Kind: kind, Path: req.Path, Cause: err}
	}
	return fileReply(req, reply)
}

func fileReply(req, reply protocol.Message) (*protocol.FileResult, error) {
	switch reply.Kind {
	case protocol.KindFileResult:
		if reply.File == nil {
			return nil, errors.New("device returned an empty file result")
		}
		return reply.File, nil
	case protocol.KindReject:
		return nil, rejectError(reply)
	case protocol.KindError:
		// This is what today's agent answers to a frame kind it has never heard
		// of (internal/agent/agent.go, the request loop's default branch).
		if strings.HasPrefix(reply.Reason, "unknown request") {
			return nil, &UnsupportedError{Kind: req.Kind}
		}
		return nil, &FileOpError{Reason: reply.Reason, Result: reply.File}
	default:
		return nil, fmt.Errorf("unexpected device reply: %s", reply.Kind)
	}
}

// FileOpError is a device-side refusal of a read or an edit. Result carries the
// numbers the refusal is about — the file's current hash, the occurrence count —
// so a caller can act on them instead of parsing the message.
type FileOpError struct {
	Reason string
	Result *protocol.FileResult
}

func (e *FileOpError) Error() string { return e.Reason }

// agentVersion asks the target for its version, returning "" if it cannot say.
// It is decoration on an error that is already decided, so it is given a short
// budget of its own: naming the version must never be what makes the caller
// wait.
func (c *Client) agentVersion(ctx context.Context, target string) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), versionLookupTimeout)
	defer cancel()
	st, err := c.Status(ctx, target)
	if err != nil {
		return ""
	}
	return st.Version
}

// versionLookupTimeout bounds the best-effort version lookup on the
// unsupported-agent path.
const versionLookupTimeout = 5 * time.Second

// hasWindowsDrive reports whether p starts with a drive letter, the other shape
// an absolute path takes on a device this controller may never have seen.
func hasWindowsDrive(p string) bool {
	if len(p) < 3 || p[1] != ':' {
		return false
	}
	c := p[0]
	if (p[2] != '\\' && p[2] != '/') || !(('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')) {
		return false
	}
	return true
}
