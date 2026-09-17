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
	Target string
	Path   string // absolute on the device
	Offset int    // 1-based first line; 0 means line 1
	Limit  int    // max lines; 0 means protocol.DefaultReadLines
}

// EditRequest asks a device to replace text inside one file. It takes either a
// single Old/New pair or a batch of Edits, never both.
type EditRequest struct {
	Target      string
	Path        string // absolute on the device
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
	Target  string
	Path    string // absolute on the device; missing parent directories are created
	Content string // the whole new text of the file, UTF-8
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
	case protocol.KindFileWrite:
		return "write"
	default:
		return "read/edit"
	}
}

// ReadFile returns a line range of a file on the target device.
func (c *Client) ReadFile(ctx context.Context, req ReadRequest) (*ReadResult, error) {
	res, err := c.fileOp(ctx, req.Target, protocol.Message{
		Kind:   protocol.KindFileRead,
		Path:   req.Path,
		Offset: int64(req.Offset),
		Limit:  req.Limit,
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
		Kind:    protocol.KindFileWrite,
		Path:    req.Path,
		Content: req.Content,
	})
	if err != nil {
		return nil, err
	}
	return &WriteResult{Created: res.Created, SizeBytes: res.SizeBytes, SHA256: res.SHA256}, nil
}

// fileOp dials the target, sends one file_read/file_edit request and returns the
// device's result.
func (c *Client) fileOp(ctx context.Context, target string, req protocol.Message) (*protocol.FileResult, error) {
	if !strings.HasPrefix(req.Path, "/") && !hasWindowsDrive(req.Path) {
		return nil, fmt.Errorf("path %q must be absolute on the device (~ is not expanded)", req.Path)
	}
	conn, err := c.connect(ctx, target)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	defer wsconn.CloseOnCancel(ctx, conn)()

	res, err := fileOpOver(conn, req)
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
		// An agent old enough to have no default branch at all drops the
		// session rather than answering. Read that as the same thing.
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, &UnsupportedError{Kind: req.Kind}
		}
		return nil, err
	}
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
