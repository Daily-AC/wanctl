package server

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"wanctl/internal/protocol"
)

const fileChunk = 64 << 10

// HandleFilePut receives an uploaded file and writes it beneath policyRoot.
func HandleFilePut(conn *tls.Conn, m protocol.Message, policyRoot string) {
	handleFilePut(conn, m, policyRoot, protocol.MaxFileSize)
}

func handleFilePut(conn io.ReadWriter, m protocol.Message, policyRoot string, maxSize int64) {
	if m.Size < 0 {
		protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: "upload size must not be negative"})
		return
	}
	if m.Size > maxSize {
		protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: fmt.Sprintf("upload size %d exceeds limit %d", m.Size, maxSize)})
		return
	}
	upload, err := newPendingUpload(policyRoot, m.Path, os.FileMode(m.Mode).Perm())
	if err != nil {
		protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: err.Error()})
		return
	}
	defer upload.abort()
	// Acknowledge; controller now streams FrameData until an EOF control frame.
	if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindOK}); err != nil {
		return
	}
	var written int64
	for {
		t, payload, err := protocol.ReadFrame(conn)
		if err != nil {
			return
		}
		switch t {
		case protocol.FrameData:
			if int64(len(payload)) > m.Size-written || int64(len(payload)) > maxSize-written {
				protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: "upload exceeds declared size or server limit"})
				return
			}
			n, werr := upload.file.Write(payload)
			if werr != nil {
				protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: werr.Error()})
				return
			}
			if n != len(payload) {
				protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: io.ErrShortWrite.Error()})
				return
			}
			written += int64(len(payload))
		case protocol.FrameJSON:
			control, err := protocol.DecodeMessage(payload)
			if err != nil || control.Kind != protocol.KindEOF {
				protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: "expected upload EOF control frame"})
				return
			}
			if written != m.Size {
				protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: fmt.Sprintf("upload size mismatch: got %d, want %d", written, m.Size)})
				return
			}
			if err := upload.commit(); err != nil {
				protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: err.Error()})
				return
			}
			protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindOK, Size: written})
			return
		default:
			protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: "unexpected frame during upload"})
			return
		}
	}
}

type pendingUpload struct {
	dir        *os.Root
	file       *os.File
	tempName   string
	targetName string
}

func newPendingUpload(policyRoot, path string, perm os.FileMode) (*pendingUpload, error) {
	rootPath, name, err := rootedName(policyRoot, path)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	parent, err := root.OpenRoot(filepath.Dir(name))
	root.Close()
	if err != nil {
		return nil, err
	}
	targetName := filepath.Base(name)
	if info, err := parent.Lstat(targetName); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			parent.Close()
			return nil, fmt.Errorf("refusing symbolic link %q", path)
		}
		if !info.Mode().IsRegular() {
			parent.Close()
			return nil, fmt.Errorf("refusing non-regular file %q", path)
		}
	} else if !os.IsNotExist(err) {
		parent.Close()
		return nil, err
	}
	if perm == 0 {
		perm = 0o644
	}
	for range 100 {
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			parent.Close()
			return nil, err
		}
		tempName := ".wanctl-upload-" + hex.EncodeToString(random[:])
		f, err := parent.OpenFile(tempName, secureOpenFlags|os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			parent.Close()
			return nil, err
		}
		if err := f.Chmod(perm); err != nil {
			f.Close()
			parent.Remove(tempName)
			parent.Close()
			return nil, err
		}
		return &pendingUpload{dir: parent, file: f, tempName: tempName, targetName: targetName}, nil
	}
	parent.Close()
	return nil, fmt.Errorf("could not allocate upload temporary file")
}

func (u *pendingUpload) commit() error {
	if err := u.file.Sync(); err != nil {
		return err
	}
	if err := u.file.Close(); err != nil {
		return err
	}
	u.file = nil
	if err := u.dir.Rename(u.tempName, u.targetName); err != nil {
		return err
	}
	u.tempName = ""
	return nil
}

func (u *pendingUpload) abort() {
	if u.file != nil {
		u.file.Close()
		u.file = nil
	}
	if u.tempName != "" {
		u.dir.Remove(u.tempName)
		u.tempName = ""
	}
	if u.dir != nil {
		u.dir.Close()
		u.dir = nil
	}
}

// HandleFileGet streams a regular file beneath policyRoot to the controller.
func HandleFileGet(conn *tls.Conn, m protocol.Message, policyRoot string) {
	f, err := openPolicyFile(policyRoot, m.Path)
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
		Mode: uint32(info.Mode().Perm()),
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

// openPolicyFile binds the policy decision to the open itself. os.Root resolves
// every path component beneath an open directory handle, so a concurrent
// symlink replacement cannot redirect the operation outside the allowed root.
func openPolicyFile(policyRoot, path string) (*os.File, error) {
	rootPath, name, err := rootedName(policyRoot, path)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	if info, err := root.Lstat(name); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("refusing symbolic link %q", path)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("refusing non-regular file %q", path)
		}
	} else {
		return nil, err
	}

	f, err := root.OpenFile(name, secureOpenFlags|os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("refusing non-regular file %q", path)
	}
	return f, nil
}

func rootedName(policyRoot, path string) (string, string, error) {
	target, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	rootPath := policyRoot
	if rootPath == "" {
		rootPath = filepath.VolumeName(target) + string(filepath.Separator)
	} else {
		rootPath, err = filepath.Abs(rootPath)
		if err != nil {
			return "", "", err
		}
	}
	rel, err := filepath.Rel(rootPath, target)
	if err != nil {
		return "", "", err
	}
	if rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path %q is outside policy root %q", path, policyRoot)
	}
	return rootPath, rel, nil
}
