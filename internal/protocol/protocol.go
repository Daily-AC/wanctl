// Package protocol defines the wire format spoken between the lanctl client and
// server over a TLS connection.
//
// The framing is deliberately tiny: every frame is
//
//	[1 byte type][4 byte big-endian length][length bytes payload]
//
// Control messages (handshake, exec request, exit status, errors) are JSON and
// travel in FrameJSON frames. Bulk bytes (command output, file contents) travel
// in FrameStdout / FrameStderr / FrameData frames with a raw payload so we don't
// pay a base64 tax on every chunk of output.
package protocol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// FrameType identifies what a frame carries.
type FrameType byte

const (
	FrameJSON   FrameType = 1 // a control Message, JSON-encoded
	FrameStdout FrameType = 2 // raw stdout bytes from a command
	FrameStderr FrameType = 3 // raw stderr bytes from a command
	FrameData   FrameType = 5 // raw file content bytes
)

// MaxFrame caps a single frame payload (16 MiB) to bound memory per read.
const MaxFrame = 16 << 20

// MaxFileSize is the largest file accepted by one upload (1 GiB).
const MaxFileSize int64 = 1 << 30

// Message kinds for FrameJSON control frames.
const (
	KindHello     = "hello"      // client -> server, opening greeting
	KindExec      = "exec"       // client -> server, run a command
	KindCancel    = "cancel"     // client -> server, abort the command running on this stream
	KindExecAsync = "exec_async" // client -> server, start a background job, return its id
	KindExecPoll  = "exec_poll"  // client -> server, fetch a background job's new output + status
	KindExit      = "exit"       // server -> client, command finished
	KindError     = "error"      // either direction, fatal request error
	KindReject    = "reject"     // server -> client, pairing/authz denied
	KindOK        = "ok"         // generic acknowledgement
	KindFilePut   = "file_put"   // client -> server, begin upload
	KindFileGet   = "file_get"   // client -> server, request download
	KindFileMeta  = "file_meta"  // server -> client, download metadata

	// Native file inspection and patching. file_get/file_put move whole files
	// and file_read/file_edit address their contents: a line range out, a
	// string replacement in. Both are implemented in Go on the device, so they
	// behave the same on Linux, macOS, Windows and Android instead of inheriting
	// whatever cat/sed/echo do in that device's shell.
	KindFileRead   = "file_read"   // client -> server, read a line range
	KindFileEdit   = "file_edit"   // client -> server, replace a string in place
	KindFileWrite  = "file_write"  // client -> server, create or overwrite a text file
	KindFileResult = "file_result" // server -> client, result of a read, an edit or a write

	KindEOF    = "eof"    // end of a FrameData stream
	KindLogs   = "logs"   // client -> server, request event-log lines
	KindStatus = "status" // client -> server, request read-only agent status

	// console session (portal <-> device control plane)
	KindConsoleHello  = "console_hello"  // portal -> device, opens a control-plane session
	KindConsoleState  = "console_state"  // both: request state / device replies with Data
	KindDecide        = "decide"         // portal -> device, resolve a pending approval
	KindRuleAdd       = "rule_add"       // portal -> device, add an allow-list rule
	KindRuleRm        = "rule_rm"        // portal -> device, remove rule by Index
	KindModeSet       = "mode_set"       // portal -> device, set normal/bypass
	KindApprovalNotif = "approval_notif" // device -> portal, UNSOLICITED: pending set changed
	KindPairDecide    = "pair_decide"    // portal -> device, trust/deny a pending controller pairing
	KindTrustRevoke   = "trust_revoke"   // portal -> device, drop a trusted controller by fingerprint
	KindADBPair       = "adb_pair"       // console administrator pairs this Android installation with local adbd
	KindTimeoutSet    = "timeout_set"    // portal -> device, set how long an approval waits (TimeoutSec; 0 = default)
)

// Message is the JSON body of a FrameJSON frame. Fields are reused across kinds;
// only those relevant to a given Kind are populated.
type Message struct {
	Kind string `json:"kind"`

	// hello
	Role    string `json:"role,omitempty"`
	Name    string `json:"name,omitempty"`
	Label   string `json:"label,omitempty"` // controller self-description shown at pairing/audit
	Version string `json:"version,omitempty"`

	// exec
	Command string `json:"command,omitempty"`
	OneShot bool   `json:"oneshot,omitempty"`
	Cwd     string `json:"cwd,omitempty"` // working directory for the command (policy scope)

	// exec: when the output passes SpillAfter bytes, the device keeps the whole
	// thing in a file under its temp dir and names that file in the exit
	// message, so a controller that can only carry a truncated tail back to its
	// caller can still say where the rest is. Zero — every controller that does
	// not ask — means nothing is ever written. An agent from before this field
	// existed decodes it into nothing and answers with no Path, which is what
	// tells the controller to say the full output was not kept.
	SpillAfter int64 `json:"spill_after,omitempty"`

	// exec exit: how much of the output the device's own copy actually holds,
	// when that is less than Size. A spill file is capped, so a truly enormous
	// output leaves the device holding its first MaxSpillBytes and the
	// controller showing its last; saying so is what keeps a caller from
	// grepping the file for a line that was never written to it.
	SpillKept int64 `json:"spill_kept,omitempty"`

	// exec: run through an elevation channel (Android; see internal/elevate).
	// Elevate is the request; Via optionally pins one channel ("su",
	// "adb") instead of letting the device pick. Both are omitted by every
	// controller that does not ask for elevation, so an older device rejects
	// the command it cannot honour rather than silently running it unelevated:
	// Elevate arrives as an unknown field there, and the exit path below treats
	// a device that never echoes ElevatedVia as one that did not elevate.
	Elevate bool   `json:"elevate,omitempty"`
	Via     string `json:"via,omitempty"`

	// exit: which channel actually ran an elevated command. The controller
	// prints it, and its absence on an elevated request is an error rather
	// than a shrug.
	ElevatedVia string `json:"elevated_via,omitempty"`

	// exit
	Code int `json:"code,omitempty"`

	// exec_async / exec_poll (background jobs)
	JobID   string `json:"job_id,omitempty"`  // server->client on start; client->server on poll
	Offset  int64  `json:"offset,omitempty"`  // poll: bytes already consumed (req) / new total length (resp)
	Running bool   `json:"running,omitempty"` // poll resp: true = job still running (Code not yet meaningful)

	// error / reject
	Reason string `json:"reason,omitempty"`

	// reject: when a controller's first connection lands on an unknown
	// fingerprint and the device couldn't get a live human decision, the device
	// fills PairingURL so the controller can surface it to its operator. Opening
	// the URL lands on the portal's pair-confirmation page (after SSO if needed)
	// and a single click trusts the controller; the next dial then goes through.
	PairingURL string `json:"pairing_url,omitempty"`

	// file_put / file_get / file_meta. On an exit message Path and Size instead
	// name the spill file the device wrote for an over-long command output.
	Path string `json:"path,omitempty"`
	Size int64  `json:"size,omitempty"`
	Mode uint32 `json:"mode,omitempty"` // file permission bits

	// file_read reuses Path, Offset (1-based first line) and Limit (max lines).
	// file_edit reuses Path and adds the replacement itself.
	Old         string `json:"old,omitempty"`             // edit: the string to find; never empty
	New         string `json:"new,omitempty"`             // edit: what to put there; empty means delete
	All         bool   `json:"all,omitempty"`             // edit: replace every occurrence instead of refusing on >1
	ExpectedSHA string `json:"expected_sha256,omitempty"` // edit: refuse unless the file still hashes to this
	Content     string `json:"content,omitempty"`         // write: the whole new text of the file

	// Edits is the other shape a file_edit takes: several replacements applied
	// to the same file in one request, each matching the text as it was BEFORE
	// any of them ran. It is exclusive with Old/New — a request carrying both
	// is refused rather than resolved by precedence — and All has no meaning
	// here, because an entry that matches more than once is the error.
	//
	// One request rather than several is not only a round trip saved: the whole
	// set is checked before anything is written, so a batch that would half
	// apply leaves the file exactly as it was.
	Edits []FileEdit `json:"edits,omitempty"`

	// File carries the outcome of a file_read or file_edit. It rides on a
	// file_result message and also on the error that refuses one, because the
	// numbers a caller needs to recover — the file's current hash, how many
	// occurrences were actually found — are exactly what the refusal is about.
	File *FileResult `json:"file,omitempty"`

	// logs
	LogType string `json:"log_type,omitempty"`
	Grep    string `json:"grep,omitempty"`
	Since   string `json:"since,omitempty"` // RFC3339
	Limit   int    `json:"limit,omitempty"`

	// console session control plane
	Verdict     string          `json:"verdict,omitempty"`      // decide: y/a/g/n
	ApprovalID  string          `json:"approval_id,omitempty"`  // decide: which pending
	Approver    string          `json:"approver,omitempty"`     // decide: "portal:<email>" for audit
	ConsoleMode string          `json:"console_mode,omitempty"` // mode_set: normal/bypass
	RuleKind    string          `json:"rule_kind,omitempty"`    // rule_add: exec/read/write
	Pattern     string          `json:"pattern,omitempty"`      // rule_add: command or dir
	Dir         string          `json:"dir,omitempty"`          // rule_add: exec dir scope
	Scope       string          `json:"scope,omitempty"`        // rule_add: dir/global
	Index       int             `json:"index,omitempty"`        // rule_rm
	TimeoutSec  int             `json:"timeout_sec,omitempty"`  // timeout_set: approval wait in seconds (0 = restore default)
	PairPort    int             `json:"pair_port,omitempty"`
	PairCode    string          `json:"pair_code,omitempty"`
	FP          string          `json:"fp,omitempty"`   // pair_decide: controller fingerprint
	Data        json.RawMessage `json:"data,omitempty"` // console_state / approval_notif payload
}

// FileEdit is one replacement inside a multi-edit file_edit request.
type FileEdit struct {
	Old string `json:"old"` // the text to find; must occur exactly once in the original
	New string `json:"new"` // what to put there; empty deletes Old
}

// File operation limits. MaxReadBytes bounds what one file_read returns, so a
// caller that asks for a line range inside a huge file gets a bounded reply
// rather than a 16 MiB frame; MaxEditBytes bounds the file a file_edit will
// rewrite, matching the inline push cap. DefaultReadLines is the line budget a
// caller that names none gets.
const (
	MaxReadBytes     = 256 << 10 // 256 KiB of returned content
	MaxEditBytes     = 8 << 20   // 8 MiB, the largest file an edit will rewrite
	DefaultReadLines = 2000

	// MaxBatchEdits caps the entries in one file_edit. Each entry is searched
	// for across the whole file, so the work a single request can ask for grows
	// with the product of the two; without a cap an authenticated controller
	// could spend a device's CPU with one small frame. Sixty-four is far more
	// than a human-sized patch and far less than a weapon.
	MaxBatchEdits = 64
)

// FileResult is what a device reports after a file_read or a file_edit.
type FileResult struct {
	// read
	Content    string `json:"content,omitempty"`     // the requested lines, line endings as stored
	TotalLines int    `json:"total_lines,omitempty"` // lines in the whole file
	FirstLine  int    `json:"first_line,omitempty"`  // 1-based number of the first returned line
	LastLine   int    `json:"last_line,omitempty"`   // 1-based number of the last returned line
	Truncated  bool   `json:"truncated,omitempty"`   // the byte cap cut the requested range short
	// LongLine names a line that does not fit in MaxReadBytes on its own, whose
	// first MaxReadBytes are returned as a prefix. It is the one case where
	// asking again from the next line would not make progress, so every surface
	// tells the caller to reach for a different tool instead of paging on.
	LongLine int `json:"long_line,omitempty"`

	// edit
	Replaced    int `json:"replaced,omitempty"`    // occurrences actually replaced
	Occurrences int `json:"occurrences,omitempty"` // occurrences found (populated on a refusal)

	// write
	Created bool `json:"created,omitempty"` // the file did not exist before this write

	// both
	SizeBytes int64  `json:"size_bytes"`       // the whole file's size, after an edit
	SHA256    string `json:"sha256,omitempty"` // the whole file's hash, after an edit
}

// WriteFrame writes a single framed payload.
func WriteFrame(w io.Writer, t FrameType, payload []byte) error {
	if len(payload) > MaxFrame {
		return fmt.Errorf("protocol: frame too large: %d", len(payload))
	}
	var hdr [5]byte
	hdr[0] = byte(t)
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

// ReadFrame reads a single framed payload.
func ReadFrame(r io.Reader) (FrameType, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxFrame {
		return 0, nil, fmt.Errorf("protocol: frame too large: %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	return FrameType(hdr[0]), buf, nil
}

// WriteMessage encodes m as a FrameJSON frame.
func WriteMessage(w io.Writer, m Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return WriteFrame(w, FrameJSON, b)
}

// DecodeMessage parses a control message from an already-read JSON payload.
func DecodeMessage(payload []byte) (Message, error) {
	var m Message
	err := json.Unmarshal(payload, &m)
	return m, err
}

// ReadMessage reads the next frame and requires it to be a control message.
func ReadMessage(r io.Reader) (Message, error) {
	t, b, err := ReadFrame(r)
	if err != nil {
		return Message{}, err
	}
	if t != FrameJSON {
		return Message{}, fmt.Errorf("protocol: expected control frame, got type %d", t)
	}
	var m Message
	if err := json.Unmarshal(b, &m); err != nil {
		return Message{}, err
	}
	return m, nil
}

// ErrClosed is returned when the peer closes the connection cleanly mid-stream.
var ErrClosed = errors.New("protocol: connection closed")
