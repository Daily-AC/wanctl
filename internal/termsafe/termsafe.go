// Package termsafe makes text that came from somewhere else safe to put on a
// terminal.
//
// A terminal acts on some of the bytes it is sent: ESC starts a sequence that
// can move the cursor, erase and rewrite what is already on screen, retitle the
// window, or reach whatever else the terminal emulator implements; the C1
// controls (U+0080–U+009F) do the same in one character on terminals that
// honour them. A device's command output, file contents and error messages are
// chosen by whoever controls that device, so shown raw they can make a
// controller's terminal display something other than what the device sent.
//
// Everything here replaces such characters with a visible escape in Go's
// quoting style — \x1b, \u009b — and leaves printable text alone, including
// every valid non-ASCII character. Bytes that are not valid UTF-8 are escaped
// too: on the UTF-8 terminals wanctl prints to they are not text, and a lone
// byte in 0x80–0x9F is a C1 control to a terminal that reads 8-bit input.
//
// This is for display only. Anything written to a pipe or a file must keep its
// bytes, so the choice of where to apply it belongs to the caller.
package termsafe

import (
	"io"
	"unicode/utf8"
)

// Escape returns s with terminal control characters made visible. Newline and
// tab survive; a carriage return survives only as part of CRLF, because on its
// own it lets the text overwrite what was printed before it on the same line.
// Meant for a message a person reads, such as an error from a device.
func Escape(s string) string {
	b := []byte(s)
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		if b[i] == '\r' && i+1 < len(b) && b[i+1] == '\n' {
			out = append(out, '\r', '\n')
			i += 2
			continue
		}
		var n int
		out, n = appendOne(out, b[i:], false)
		i += n
	}
	return string(out)
}

// Writer streams output to a terminal with control characters made visible.
// Newline, carriage return and tab pass through: output that redraws its own
// line with \r, like a progress bar, keeps working. A character split across
// two writes is held until the rest arrives, so multi-byte text is never
// escaped by accident; Flush writes out anything still held.
type Writer struct {
	w    io.Writer
	held []byte
}

// NewWriter returns a Writer that escapes into w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Write escapes p into the underlying writer. It reports len(p) on success:
// the caller's bytes were all consumed, whatever size they became.
func (t *Writer) Write(p []byte) (int, error) {
	data := p
	if len(t.held) > 0 {
		data = append(t.held, p...)
		t.held = nil
	}
	tail := incompleteTail(data)
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data)-tail; {
		var n int
		out, n = appendOne(out, data[i:len(data)-tail], true)
		i += n
	}
	if tail > 0 {
		t.held = append([]byte(nil), data[len(data)-tail:]...)
	}
	if len(out) > 0 {
		if _, err := t.w.Write(out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Flush writes anything held back waiting for the rest of a character. At the
// end of the stream that rest is not coming, so the bytes are escaped.
func (t *Writer) Flush() error {
	if len(t.held) == 0 {
		return nil
	}
	var out []byte
	for i := 0; i < len(t.held); {
		var n int
		out, n = appendOne(out, t.held[i:], true)
		i += n
	}
	t.held = nil
	_, err := t.w.Write(out)
	return err
}

// appendOne appends the escaped form of the first character of b and reports
// how many bytes of b it consumed.
func appendOne(out, b []byte, keepCR bool) ([]byte, int) {
	r, size := utf8.DecodeRune(b)
	switch {
	case r == utf8.RuneError && size <= 1:
		return appendHex(out, `\x`, uint32(b[0]), 2), 1
	case r == '\n' || r == '\t' || (r == '\r' && keepCR):
		return append(out, byte(r)), 1
	case r < 0x20 || r == 0x7f:
		return appendHex(out, `\x`, uint32(r), 2), 1
	case r >= 0x80 && r <= 0x9f:
		return appendHex(out, `\u`, uint32(r), 4), size
	}
	return append(out, b[:size]...), size
}

func appendHex(out []byte, prefix string, v uint32, digits int) []byte {
	const hex = "0123456789abcdef"
	out = append(out, prefix...)
	for shift := (digits - 1) * 4; shift >= 0; shift -= 4 {
		out = append(out, hex[(v>>uint(shift))&0xf])
	}
	return out
}

// incompleteTail is how many bytes at the end of b begin a UTF-8 character
// whose remaining bytes have not arrived yet.
func incompleteTail(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return len(b) - i
			}
			return 0
		}
	}
	return 0
}
