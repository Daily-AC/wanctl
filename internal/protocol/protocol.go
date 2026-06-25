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

// Message kinds for FrameJSON control frames.
const (
	KindHello    = "hello"     // client -> server, opening greeting
	KindExec     = "exec"      // client -> server, run a command
	KindExit     = "exit"      // server -> client, command finished
	KindError    = "error"     // either direction, fatal request error
	KindReject   = "reject"    // server -> client, pairing/authz denied
	KindOK       = "ok"        // generic acknowledgement
	KindFilePut  = "file_put"  // client -> server, begin upload
	KindFileGet  = "file_get"  // client -> server, request download
	KindFileMeta = "file_meta" // server -> client, download metadata
	KindEOF      = "eof"       // end of a FrameData stream
	KindLogs     = "logs"      // client -> server, request event-log lines

	// console session (portal <-> device control plane)
	KindConsoleHello  = "console_hello"  // portal -> device, opens a control-plane session
	KindConsoleState  = "console_state"  // both: request state / device replies with Data
	KindDecide        = "decide"         // portal -> device, resolve a pending approval
	KindRuleAdd       = "rule_add"       // portal -> device, add an allow-list rule
	KindRuleRm        = "rule_rm"        // portal -> device, remove rule by Index
	KindModeSet       = "mode_set"       // portal -> device, set normal/bypass
	KindApprovalNotif = "approval_notif" // device -> portal, UNSOLICITED: pending set changed
	KindPairDecide    = "pair_decide"    // portal -> device, trust/deny a pending controller pairing
)

// Message is the JSON body of a FrameJSON frame. Fields are reused across kinds;
// only those relevant to a given Kind are populated.
type Message struct {
	Kind string `json:"kind"`

	// hello
	Role    string `json:"role,omitempty"`
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`

	// exec
	Command string `json:"command,omitempty"`
	OneShot bool   `json:"oneshot,omitempty"`
	Cwd     string `json:"cwd,omitempty"` // working directory for the command (policy scope)

	// exit
	Code int `json:"code,omitempty"`

	// error / reject
	Reason string `json:"reason,omitempty"`

	// file_put / file_get / file_meta
	Path string `json:"path,omitempty"`
	Size int64  `json:"size,omitempty"`
	Mode uint32 `json:"mode,omitempty"` // file permission bits

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
	FP          string          `json:"fp,omitempty"`           // pair_decide: controller fingerprint
	Data        json.RawMessage `json:"data,omitempty"`         // console_state / approval_notif payload
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
