package server

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"wanctl/internal/protocol"
)

// This file implements file_read and file_edit: reading a line range out of a
// file and replacing a string inside one, in Go, on the device.
//
// The point is that neither goes through a shell. cat, sed and echo are four
// different programs across the four platforms a wanctl agent runs on, and
// driving them over exec means the caller's text is first shell source: a $ or a
// backtick in a replacement is a quoting hazard, a long file comes back cut off
// by whatever the caller remembered to pipe it through, and a patch is really a
// whole-file overwrite that loses anything written since the read.
//
// Both operations bind their filesystem access through the same os.Root-rooted
// helpers as file_get/file_put (openPolicyFile, newPendingUpload), so the policy
// decision constrains the open itself rather than a path string checked earlier.

// sniffLen is how much of a file is inspected to decide whether it is text.
const sniffLen = 8 << 10

// errNotText refuses a file that is not UTF-8 text.
var errNotText = errors.New("not a UTF-8 text file")

// HandleFileRead returns a line range of a regular file beneath policyRoot,
// along with the whole file's line count, size and hash.
func HandleFileRead(conn *tls.Conn, m protocol.Message, policyRoot string) {
	handleFileRead(conn, m, policyRoot)
}

func handleFileRead(conn io.ReadWriter, m protocol.Message, policyRoot string) {
	f, err := openPolicyFile(policyRoot, m.Path)
	if err != nil {
		writeFileError(conn, err.Error(), nil)
		return
	}
	defer f.Close()

	res, err := readLineRange(f, int(m.Offset), m.Limit)
	if errors.Is(err, errNotText) {
		writeFileError(conn, fmt.Sprintf(
			"%q is not a UTF-8 text file; download it with pull, or process it on the device with exec", m.Path), nil)
		return
	}
	if err != nil {
		writeFileError(conn, err.Error(), nil)
		return
	}
	protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindFileResult, Path: m.Path, File: res})
}

// readLineRange streams r once, hashing every byte and counting every line while
// collecting the requested range. Streaming rather than slurping is what lets a
// read address line 4,990,000 of a log without the device holding the log in
// memory; only the collected range is buffered, and it is capped.
func readLineRange(r io.Reader, offset, limit int) (*protocol.FileResult, error) {
	if offset <= 0 {
		offset = 1
	}
	if limit <= 0 {
		limit = protocol.DefaultReadLines
	}
	c := &lineCollector{from: offset, to: offset + limit - 1, line: 1}

	sum := sha256.New()
	var (
		size     int64
		sniff    []byte
		lastByte byte
	)
	buf := make([]byte, fileChunk)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			sum.Write(chunk)
			size += int64(n)
			if len(sniff) < sniffLen {
				sniff = append(sniff, chunk[:min(n, sniffLen-len(sniff))]...)
			}
			lastByte = chunk[n-1]
			c.feed(chunk)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
	}
	if !isText(sniff, size) {
		return nil, errNotText
	}

	total := c.line - 1 // lines closed by a newline
	if size > 0 && lastByte != '\n' {
		total++ // a final line with no terminator is still a line
	}
	return &protocol.FileResult{
		Content:    c.out.String(),
		TotalLines: total,
		FirstLine:  c.first,
		LastLine:   c.last,
		Truncated:  c.truncated,
		SizeBytes:  size,
		SHA256:     hex.EncodeToString(sum.Sum(nil)),
	}, nil
}

// lineCollector accumulates the bytes of lines [from, to] as they stream past,
// copying line endings verbatim so a CRLF file reads back as CRLF.
type lineCollector struct {
	from, to    int
	line        int // line number of the next byte to be fed
	first, last int // 1-based bounds of what was actually collected
	truncated   bool
	out         strings.Builder
}

func (c *lineCollector) feed(chunk []byte) {
	for len(chunk) > 0 {
		piece := chunk
		nl := bytes.IndexByte(chunk, '\n')
		if nl >= 0 {
			piece = chunk[:nl+1]
		}
		if c.line >= c.from && c.line <= c.to && !c.truncated {
			c.take(piece)
		}
		if nl < 0 {
			return
		}
		chunk = chunk[nl+1:]
		c.line++
	}
}

func (c *lineCollector) take(piece []byte) {
	room := protocol.MaxReadBytes - c.out.Len()
	if room <= 0 {
		c.truncated = true
		return
	}
	if len(piece) > room {
		piece, c.truncated = piece[:room], true
	}
	c.out.Write(piece)
	if c.first == 0 {
		c.first = c.line
	}
	c.last = c.line
}

// isText reports whether the sniffed prefix looks like UTF-8 text: no NUL bytes
// and no invalid sequences. When the prefix is a cut of a longer file, a rune
// split by the cut is not corruption, so up to three trailing bytes are dropped
// before the verdict.
func isText(sniff []byte, size int64) bool {
	if bytes.IndexByte(sniff, 0) >= 0 {
		return false
	}
	if int64(len(sniff)) < size {
		for range 3 {
			if utf8.Valid(sniff) || len(sniff) == 0 {
				break
			}
			sniff = sniff[:len(sniff)-1]
		}
	}
	return utf8.Valid(sniff)
}

// HandleFileEdit replaces a string inside a regular file beneath policyRoot and
// writes the result atomically, or refuses and leaves the file untouched.
func HandleFileEdit(conn *tls.Conn, m protocol.Message, policyRoot string) {
	handleFileEdit(conn, m, policyRoot)
}

func handleFileEdit(conn io.ReadWriter, m protocol.Message, policyRoot string) {
	if m.Old == "" {
		writeFileError(conn, "edit needs a non-empty 'old' string to find", nil)
		return
	}
	f, err := openPolicyFile(policyRoot, m.Path)
	if err != nil {
		writeFileError(conn, err.Error(), nil)
		return
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		writeFileError(conn, err.Error(), nil)
		return
	}
	original, err := io.ReadAll(io.LimitReader(f, protocol.MaxEditBytes+1))
	f.Close()
	if err != nil {
		writeFileError(conn, err.Error(), nil)
		return
	}
	if len(original) > protocol.MaxEditBytes {
		writeFileError(conn, fmt.Sprintf(
			"%q is over the %d-byte (8 MiB) edit limit; patch it with exec on the device, or replace the whole file with push",
			m.Path, int64(protocol.MaxEditBytes)), nil)
		return
	}

	current := sha256Hex(original)
	if m.ExpectedSHA != "" && !strings.EqualFold(m.ExpectedSHA, current) {
		writeFileError(conn, fmt.Sprintf(
			"%q changed since it was read: expected sha256 %s, found %s. Nothing was written; read the file again and redo the edit against its current text",
			m.Path, m.ExpectedSHA, current),
			&protocol.FileResult{SHA256: current, SizeBytes: int64(len(original))})
		return
	}

	text := string(original)
	count := strings.Count(text, m.Old)
	switch {
	case count == 0:
		writeFileError(conn, fmt.Sprintf("old string not found in %q; nothing was written", m.Path),
			&protocol.FileResult{SHA256: current, SizeBytes: int64(len(original))})
		return
	case count > 1 && !m.All:
		writeFileError(conn, fmt.Sprintf(
			"old string occurs %d times in %q; nothing was written. Include enough surrounding text to match exactly once, or pass all=true to replace every occurrence",
			count, m.Path),
			&protocol.FileResult{Occurrences: count, SHA256: current, SizeBytes: int64(len(original))})
		return
	}

	replaced := 1
	if m.All {
		replaced = count
	}
	updated := strings.Replace(text, m.Old, m.New, replaced)

	// Write through the upload path: a temp file in the same directory, opened
	// under the same os.Root as the target and renamed over it. A reader sees
	// either the old file or the new one, never a half-written one, and the
	// original mode is carried across.
	upload, err := newPendingUpload(policyRoot, m.Path, info.Mode().Perm())
	if err != nil {
		writeFileError(conn, err.Error(), nil)
		return
	}
	defer upload.abort()
	if _, err := upload.file.Write([]byte(updated)); err != nil {
		writeFileError(conn, err.Error(), nil)
		return
	}
	if err := upload.commit(); err != nil {
		writeFileError(conn, err.Error(), nil)
		return
	}

	protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindFileResult, Path: m.Path, File: &protocol.FileResult{
		Replaced:    replaced,
		Occurrences: count,
		SizeBytes:   int64(len(updated)),
		SHA256:      sha256Hex([]byte(updated)),
	}})
}

func writeFileError(conn io.ReadWriter, reason string, res *protocol.FileResult) {
	protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: reason, File: res})
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
