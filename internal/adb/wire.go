// Package adb speaks the Android Debug Bridge wire protocol, so the wanctl
// agent can connect to the adbd running on its own device over loopback and
// execute commands as uid 2000 (shell) rather than as its own app uid.
//
// Why this is written rather than shipped: the only prebuilt Android/arm64 adb
// in existence needs seven external libraries, two of which cannot be extracted
// from an APK under Android's lib*.so rule, and vendoring a third-party
// prebuilt into an APK whose update story rests on two independent signature
// chains agreeing would contradict that story. ADR 0004 has the full argument.
//
// The protocol is documented in AOSP's packages/modules/adb/protocol.txt and
// has been stable for over a decade.
package adb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// command is the 4-byte tag at the head of every message. The values are the
// ASCII spelling read as a little-endian uint32; deriving them rather than
// writing the hex out means a typo cannot survive compilation.
type command uint32

func cmd(s string) command {
	if len(s) != 4 {
		panic("adb: command tag must be 4 bytes")
	}
	return command(binary.LittleEndian.Uint32([]byte(s)))
}

var (
	cmdCnxn = cmd("CNXN") // connect
	cmdAuth = cmd("AUTH") // authenticate
	cmdOpen = cmd("OPEN") // open a stream
	cmdOkay = cmd("OKAY") // ready / ack
	cmdClse = cmd("CLSE") // close a stream
	cmdWrte = cmd("WRTE") // stream payload
	cmdStls = cmd("STLS") // start TLS (Android 9+, mandatory on 11+ wireless)
)

func (c command) String() string {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(c))
	for _, ch := range b {
		if ch < 0x20 || ch > 0x7e {
			return fmt.Sprintf("0x%08x", uint32(c))
		}
	}
	return string(b[:])
}

// AUTH sub-types, carried in arg0.
const (
	authToken     = 1 // device → host: 20 random bytes to sign
	authSignature = 2 // host → device: PKCS#1 v1.5 signature over the token
	authPublicKey = 3 // host → device: public key, for the "allow debugging?" prompt
)

// Protocol version and payload ceiling advertised in CNXN. These are the values
// a modern adb host sends; adbd answers with its own and the smaller wins.
const (
	// versionSkipChecksum is the protocol version from which the header's
	// checksum field is neither filled in nor verified. AOSP calls it
	// A_VERSION_SKIP_CHECKSUM.
	versionSkipChecksum = 0x01000001
	protocolVersion     = versionSkipChecksum
	maxPayload          = 1024 * 1024
)

// headerSize is six little-endian uint32s: command, arg0, arg1, length,
// checksum, magic.
const headerSize = 24

// message is one protocol frame.
type message struct {
	Command command
	Arg0    uint32
	Arg1    uint32
	Data    []byte
}

// checksum is the protocol's integrity field: a plain sum of the payload bytes,
// truncated to 32 bits. It is not a CRC despite the field's name in the C
// struct, and adbd rejects a message whose sum does not match.
func checksum(data []byte) uint32 {
	var sum uint32
	for _, b := range data {
		sum += uint32(b)
	}
	return sum
}

// write encodes a message onto w.
func (m message) write(w io.Writer) error {
	var hdr [headerSize]byte
	binary.LittleEndian.PutUint32(hdr[0:], uint32(m.Command))
	binary.LittleEndian.PutUint32(hdr[4:], m.Arg0)
	binary.LittleEndian.PutUint32(hdr[8:], m.Arg1)
	binary.LittleEndian.PutUint32(hdr[12:], uint32(len(m.Data)))
	binary.LittleEndian.PutUint32(hdr[16:], checksum(m.Data))
	// magic is the command's ones' complement. A receiver that finds otherwise
	// is not looking at a message boundary.
	binary.LittleEndian.PutUint32(hdr[20:], uint32(m.Command)^0xffffffff)
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(m.Data) == 0 {
		return nil
	}
	_, err := w.Write(m.Data)
	return err
}

// errBadMagic means the stream is out of frame, which is unrecoverable: there
// is no resynchronization point in this protocol.
var errBadMagic = errors.New("adb: message magic does not match command (stream out of sync)")

// readMessage decodes one message from r. limit bounds the payload this side is
// willing to allocate, so a corrupt or hostile length field cannot exhaust
// memory on a phone.
//
// verifyChecksum is false against any adbd speaking protocol version
// 0x01000001 or later, which is every device since Android 4-ish: those send a
// header whose checksum field is a literal zero and verify nothing on receipt.
// Measured against an android-29 emulator's adbd on 2026-08-14 — its very first
// CNXN carries checksum=0 over a 20764-byte-sum payload, so a client that
// insists on the field cannot get past the first packet. This side keeps
// *computing* the checksum on the way out, because an old adbd still checks it
// and a new one ignores it; only the verification is conditional.
func readMessage(r io.Reader, limit uint32, verifyChecksum bool) (message, error) {
	var hdr [headerSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return message{}, err
	}
	m := message{
		Command: command(binary.LittleEndian.Uint32(hdr[0:])),
		Arg0:    binary.LittleEndian.Uint32(hdr[4:]),
		Arg1:    binary.LittleEndian.Uint32(hdr[8:]),
	}
	length := binary.LittleEndian.Uint32(hdr[12:])
	want := binary.LittleEndian.Uint32(hdr[16:])
	magic := binary.LittleEndian.Uint32(hdr[20:])
	if magic != uint32(m.Command)^0xffffffff {
		return message{}, errBadMagic
	}
	if length > limit {
		return message{}, fmt.Errorf("adb: %s payload of %d bytes exceeds the %d-byte limit", m.Command, length, limit)
	}
	if length > 0 {
		m.Data = make([]byte, length)
		if _, err := io.ReadFull(r, m.Data); err != nil {
			return message{}, err
		}
		if verifyChecksum {
			if got := checksum(m.Data); got != want {
				return message{}, fmt.Errorf("adb: %s payload checksum %d does not match the header's %d", m.Command, got, want)
			}
		}
	}
	return m, nil
}
