package server

import (
	"crypto/tls"
	"io"
	"os"

	"wanctl/internal/protocol"
)

const fileChunk = 64 << 10

// HandleFilePut receives an uploaded file from the controller and writes it to
// m.Path.
func HandleFilePut(conn *tls.Conn, m protocol.Message) {
	// Default to 0644 permissions if not specified
	// (Mode is now repurposed for console control, not file permissions)
	f, err := os.OpenFile(m.Path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: err.Error()})
		return
	}
	// Acknowledge; controller now streams FrameData until an EOF control frame.
	if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindOK}); err != nil {
		f.Close()
		return
	}
	var written int64
	for {
		t, payload, err := protocol.ReadFrame(conn)
		if err != nil {
			f.Close()
			return
		}
		switch t {
		case protocol.FrameData:
			if _, werr := f.Write(payload); werr != nil {
				f.Close()
				protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: werr.Error()})
				return
			}
			written += int64(len(payload))
		case protocol.FrameJSON:
			// Expect EOF; finalize.
			f.Close()
			protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindOK, Size: written})
			return
		default:
			f.Close()
			protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: "unexpected frame during upload"})
			return
		}
	}
}

// HandleFileGet streams the file at m.Path down to the controller.
func HandleFileGet(conn *tls.Conn, m protocol.Message) {
	f, err := os.Open(m.Path)
	if err != nil {
		protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: err.Error()})
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: err.Error()})
		return
	}
	if err := protocol.WriteMessage(conn, protocol.Message{
		Kind: protocol.KindFileMeta,
		Size: info.Size(),
		// Mode field is now repurposed for console control, not file permissions
	}); err != nil {
		return
	}
	buf := make([]byte, fileChunk)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if err := protocol.WriteFrame(conn, protocol.FrameData, buf[:n]); err != nil {
				return
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return
		}
	}
	protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindEOF})
}
