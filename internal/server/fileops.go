package server

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
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

// maxReadLines caps the line budget a caller may ask for, so a limit near
// MaxInt cannot overflow the last-line number computed from it.
const maxReadLines = 1 << 20

// errNotText refuses a file that is not UTF-8 text.
var errNotText = errors.New("not a UTF-8 text file")

// HandleFileRead returns a line range of a regular file beneath policyRoot,
// along with the whole file's line count, size and hash.
func HandleFileRead(conn *tls.Conn, m protocol.Message, policyRoot string) {
	handleFileRead(conn, m, policyRoot)
}

// notTextReason is the one refusal both operations give for a file that is not
// UTF-8 text, pointing at the tools that do handle bytes.
func notTextReason(path, instead string) string {
	return fmt.Sprintf("%q is not a UTF-8 text file; %s", path, instead)
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
		writeFileError(conn, notTextReason(m.Path, "download it with pull, or process it on the device with exec"), nil)
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
	if limit > maxReadLines {
		limit = maxReadLines
	}
	to := offset + limit - 1
	if to < offset {
		to = math.MaxInt // a caller-supplied offset near MaxInt wrapped the sum
	}
	c := &lineCollector{from: offset, to: to, line: 1}

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
	c.finish()
	if !isText(sniff, size) {
		return nil, errNotText
	}
	// The sniff only ever saw the first 8 KiB. What actually goes back is
	// marshalled as a JSON string, and encoding/json silently rewrites invalid
	// UTF-8 as U+FFFD -- so a file whose first 8 KiB are ASCII and whose
	// hundredth line is not would come back quietly corrupted. Check what is
	// being returned, not a prefix of the file.
	content := c.out.String()
	if !utf8.ValidString(content) {
		return nil, errNotText
	}

	total := c.line - 1 // lines closed by a newline
	if size > 0 && lastByte != '\n' {
		total++ // a final line with no terminator is still a line
	}
	return &protocol.FileResult{
		Content:    content,
		TotalLines: total,
		FirstLine:  c.first,
		LastLine:   c.last,
		Truncated:  c.truncated,
		LongLine:   c.longLine,
		SizeBytes:  size,
		SHA256:     hex.EncodeToString(sum.Sum(nil)),
	}, nil
}

// lineCollector accumulates the bytes of lines [from, to] as they stream past,
// copying line endings verbatim so a CRLF file reads back as CRLF.
//
// It hands back whole lines only. The caller is told to continue at the line
// after the last one returned, so a range cut in the middle of a line would
// either lose that line's tail or hand it back twice -- and a model paging
// through a file has no way to notice either. A line is therefore buffered
// until its newline arrives and committed only if all of it fits.
//
// The one exception is a single line longer than the whole cap, which can never
// fit and would otherwise make paging loop forever on the same line. That line
// comes back as a prefix, with longLine naming it so every surface can say so.
type lineCollector struct {
	from, to    int
	line        int // line number of the next byte to be fed
	first, last int // 1-based bounds of what was actually collected
	truncated   bool
	done        bool // the cap was reached; collect nothing further
	longLine    int  // a line too long for the cap, returned as a prefix
	out         strings.Builder
	pending     []byte // the current line so far, itself capped
	pendingLen  int    // that line's true length, which pending may not hold
}

func (c *lineCollector) feed(chunk []byte) {
	for len(chunk) > 0 {
		piece := chunk
		nl := bytes.IndexByte(chunk, '\n')
		if nl >= 0 {
			piece = chunk[:nl+1]
		}
		if !c.done && c.line >= c.from && c.line <= c.to {
			c.buffer(piece)
			if nl >= 0 {
				c.commitLine()
			}
		}
		if nl < 0 {
			return
		}
		chunk = chunk[nl+1:]
		c.line++
	}
}

// buffer appends to the line being read, holding at most one cap's worth of it
// however long the line turns out to be.
func (c *lineCollector) buffer(piece []byte) {
	if room := protocol.MaxReadBytes - len(c.pending); room > 0 {
		c.pending = append(c.pending, piece[:min(len(piece), room)]...)
	}
	c.pendingLen += len(piece)
}

// finish commits a final line that the file ended without a newline.
func (c *lineCollector) finish() {
	if !c.done && c.pendingLen > 0 {
		c.commitLine()
	}
}

func (c *lineCollector) commitLine() {
	switch room := protocol.MaxReadBytes - c.out.Len(); {
	case c.pendingLen <= room:
		c.out.Write(c.pending)
		if c.first == 0 {
			c.first = c.line
		}
		c.last = c.line
	case c.out.Len() == 0:
		// This one line is bigger than everything a read may return. Give back
		// what fits, cut on a rune boundary so the result is still text, and
		// name the line so the caller is told to use a different tool instead
		// of asking for the same line again.
		c.out.Write(trimCutRune(c.pending))
		c.first, c.last, c.longLine = c.line, c.line, c.line
		c.truncated, c.done = true, true
	default:
		c.truncated, c.done = true, true
	}
	c.pending, c.pendingLen = c.pending[:0], 0
}

// trimCutRune drops up to three trailing bytes of a buffer cut at an arbitrary
// offset, so a multi-byte character split by the cut does not make the result
// invalid UTF-8. Bytes that were already invalid in the file survive this and
// are caught by the check on the returned content.
func trimCutRune(b []byte) []byte {
	for range 3 {
		if len(b) == 0 || utf8.Valid(b) {
			break
		}
		b = b[:len(b)-1]
	}
	return b
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
	handleFileEdit(conn, m, policyRoot, protocol.MaxEditBytes)
}

func handleFileEdit(conn io.ReadWriter, m protocol.Message, policyRoot string, maxSize int64) {
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
	original, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	f.Close()
	if err != nil {
		writeFileError(conn, err.Error(), nil)
		return
	}
	if int64(len(original)) > maxSize {
		writeFileError(conn, fmt.Sprintf(
			"%q is over the %d-byte edit limit; patch it with exec on the device, or replace the whole file with push",
			m.Path, maxSize), nil)
		return
	}
	// An edit claims to work on text files, so it has to check that it is
	// looking at one. Replacing a string inside a binary would corrupt it just
	// as surely as reading it back would mangle it.
	sniff := original
	if len(sniff) > sniffLen {
		sniff = sniff[:sniffLen]
	}
	if !isText(sniff, int64(len(original))) {
		writeFileError(conn, notTextReason(m.Path, "patch it with exec on the device, or replace the whole file with push"), nil)
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
	// Size the result before building it. strings.Replace allocates the whole
	// output up front, so `all` with a replacement longer than what it replaces
	// would ask for len(file) + count*growth bytes in one go -- on a device
	// whose agent may be the only thing keeping it reachable. Refusing here
	// also closes the gap where an edit grew a file past the limit that the
	// same file could not have been uploaded at.
	projected := int64(len(text)) + int64(replaced)*(int64(len(m.New))-int64(len(m.Old)))
	if projected > maxSize {
		writeFileError(conn, fmt.Sprintf(
			"that edit would grow %q to %d bytes, over the %d-byte edit limit; nothing was written",
			m.Path, projected, maxSize),
			&protocol.FileResult{Occurrences: count, SHA256: current, SizeBytes: int64(len(original))})
		return
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
