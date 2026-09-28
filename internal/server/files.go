package server

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"wanctl/internal/protocol"
	"wanctl/internal/transport"
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
	return newPendingUploadIn(policyRoot, path, perm, false)
}

// newPendingUploadIn is newPendingUpload with a say in whether a missing parent
// directory is an error. An upload writes where the caller pointed and nowhere
// else, so file_put keeps failing on a path whose directory does not exist —
// that is almost always a typo. file_write is the one operation that means
// "make this file exist", and creating `logs/` on the way to `logs/app.conf` is
// part of that. The directories are made under the same os.Root as the file, so
// the policy decision still constrains every component.
func newPendingUploadIn(policyRoot, path string, perm os.FileMode, mkdirParents bool) (*pendingUpload, error) {
	guard := loadProtected()
	rootPath, name, err := guard.rootedName(policyRoot, path)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	if mkdirParents {
		if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			root.Close()
			return nil, err
		}
	}
	parent, err := root.OpenRoot(filepath.Dir(name))
	root.Close()
	if err != nil {
		return nil, err
	}
	targetName := filepath.Base(name)
	existing, err := parent.Lstat(targetName)
	if err == nil {
		if existing.Mode()&os.ModeSymlink != 0 {
			parent.Close()
			return nil, fmt.Errorf("refusing symbolic link %q", path)
		}
		if !existing.Mode().IsRegular() {
			parent.Close()
			return nil, fmt.Errorf("refusing non-regular file %q", path)
		}
	} else if !os.IsNotExist(err) {
		parent.Close()
		return nil, err
	} else {
		existing = nil
	}
	// The same refusal again, on the directory actually opened rather than on
	// the path that led to it. Nothing is created in it before this passes.
	if err := guard.checkOpened(parent, existing, path); err != nil {
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
	guard := loadProtected()
	rootPath, name, err := guard.rootedName(policyRoot, path)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	// The file is opened from a handle on its own directory, so the directory
	// that is checked below is the one the file was actually found in.
	parent, err := root.OpenRoot(filepath.Dir(name))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	base := filepath.Base(name)

	if info, err := parent.Lstat(base); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("refusing symbolic link %q", path)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("refusing non-regular file %q", path)
		}
	} else {
		return nil, err
	}

	f, err := parent.OpenFile(base, secureOpenFlags|os.O_RDONLY, 0)
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
	if err := guard.checkOpened(parent, info, path); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

const (
	reasonConfigDir = "wanctl's own config dir (identity, trust, policy)"
	reasonBinary    = "the running wanctl binary"
)

// protected is what no file operation may touch: wanctl's own config dir — the
// device identity key, the namespace token, the trust and policy files — and
// the running binary.
//
// They are recognised by file identity (os.SameFile), not by comparing path
// strings. A path is only one spelling of a file, and the filesystem accepts
// many: APFS and NTFS fold case, macOS reaches the data volume through
// /System/Volumes/Data firmlinks that no symlink resolution reveals, Windows has
// 8.3 short names and ignores trailing dots. A string comparison has to predict
// every one of them and is wrong on the first it misses; asking the filesystem
// which directory a path names is right for all of them at once.
type protected struct {
	configDir  os.FileInfo   // nil if there is none
	configPath string        // its resolved path, for the fallback comparison
	configFile []os.FileInfo // what sits directly inside it
	binary     os.FileInfo   // nil if it cannot be found
	binaryPath string
}

// loadProtected reads the identities fresh for each operation: the config dir
// gains files over the agent's life (a first-time token, a new rules file), and
// a stale list would miss them.
func loadProtected() protected {
	var p protected
	if dir, err := transport.ConfigDir(); err == nil && dir != "" {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			p.configDir, p.configPath = info, longestRealPrefix(dir)
			if entries, err := os.ReadDir(dir); err == nil {
				for _, e := range entries {
					if info, err := os.Stat(filepath.Join(dir, e.Name())); err == nil && !info.IsDir() {
						p.configFile = append(p.configFile, info)
					}
				}
			}
		}
	}
	if self, err := os.Executable(); err == nil {
		if info, err := os.Stat(self); err == nil {
			p.binary, p.binaryPath = info, longestRealPrefix(self)
		}
	}
	return p
}

// protectedTarget names why target may not be transferred, or "" if it is fine.
func protectedTarget(target string) string { return loadProtected().target(target) }

// target decides from the path, before anything is opened or created: the
// target itself and every directory above it that exists are compared with the
// protected identities. Walking up is what covers a file that does not exist
// yet, and a directory that file_write would create inside the config dir.
func (p protected) target(target string) string {
	resolved := longestRealPrefix(target)
	for dir := resolved; ; {
		if info, err := os.Stat(dir); err == nil {
			if p.configDir != nil && os.SameFile(info, p.configDir) {
				return reasonConfigDir
			}
			if dir == resolved && p.binary != nil && os.SameFile(info, p.binary) {
				return reasonBinary
			}
		}
		up := filepath.Dir(dir)
		if up == dir {
			break
		}
		dir = up
	}
	// Identity needs a stat that succeeds. Where one does not, fall back to
	// comparing resolved paths the way the platform's default volume compares
	// names, so the fallback at least agrees with the filesystem about case.
	if p.configPath != "" && pathWithinOn(runtime.GOOS, resolved, p.configPath) {
		return reasonConfigDir
	}
	if p.binaryPath != "" && foldPath(runtime.GOOS, resolved) == foldPath(runtime.GOOS, p.binaryPath) {
		return reasonBinary
	}
	return ""
}

// checkOpened repeats the decision on what was actually opened: the directory
// handle the operation works in, and the file found there. The path check above
// runs before the open, and this one closes the distance between the two — a
// directory swapped in between, or a file reachable under a second name.
func (p protected) checkOpened(dir *os.Root, file os.FileInfo, path string) error {
	if p.configDir != nil {
		if info, err := dir.Stat("."); err != nil {
			return err
		} else if os.SameFile(info, p.configDir) {
			return fmt.Errorf("refusing %s: %q", reasonConfigDir, path)
		}
	}
	if file == nil {
		return nil
	}
	if p.binary != nil && os.SameFile(file, p.binary) {
		return fmt.Errorf("refusing %s: %q", reasonBinary, path)
	}
	for _, f := range p.configFile {
		if os.SameFile(file, f) {
			return fmt.Errorf("refusing %s: %q", reasonConfigDir, path)
		}
	}
	return nil
}

func pathWithin(target, dir string) bool {
	return pathWithinOn(runtime.GOOS, target, dir)
}

// pathWithinOn reports whether target is dir or beneath it, comparing names the
// way goos's default filesystem does (see foldPath).
func pathWithinOn(goos, target, dir string) bool {
	sep := "/"
	if goos == "windows" {
		sep = `\`
	}
	target, dir = foldPath(goos, target), foldPath(goos, dir)
	if target == dir {
		return true
	}
	return strings.HasPrefix(target, strings.TrimSuffix(dir, sep)+sep)
}

// foldPath lower-cases p where goos's default volumes are case-insensitive:
// NTFS on Windows, APFS on macOS. Folding on a macOS volume formatted
// case-sensitive can only refuse more, never admit more.
func foldPath(goos, p string) string {
	if goos == "windows" || goos == "darwin" || goos == "ios" {
		return strings.ToLower(p)
	}
	return p
}

// longestRealPrefix resolves symlinks on the longest existing leading portion
// of p and re-attaches the rest, so a not-yet-created file still compares
// against a resolved directory (and platform links like macOS /var ->
// /private/var do not create a false mismatch).
func longestRealPrefix(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	dir := filepath.Dir(p)
	if dir == p {
		return p
	}
	return filepath.Join(longestRealPrefix(dir), filepath.Base(p))
}

func rootedName(policyRoot, path string) (string, string, error) {
	return loadProtected().rootedName(policyRoot, path)
}

func (p protected) rootedName(policyRoot, path string) (string, string, error) {
	target, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	// The device's own identity, trust set, policy and token are never
	// legitimate transfer targets. Without this a write grant whose policy
	// root reaches the config dir (a global/bypass write) could overwrite
	// portal_admins.json to make the controller a console administrator, or
	// read key.pem to steal the device identity — an escalation independent of
	// the policy root (audit 2026-08-28, SEC-D1-01). Refused for both read and
	// write, since rootedName backs openPolicyFile and newPendingUpload alike.
	if reason := p.target(target); reason != "" {
		return "", "", fmt.Errorf("refusing %s: %q", reason, path)
	}
	rootPath := policyRoot
	if rootPath == "" {
		rootPath = unconstrainedRoot(target)
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

// unconstrainedRoot is the root to bind an open to when policy imposes none
// (a bypass-mode or global decision) — "the whole volume" everywhere except
// Android.
//
// os.Root walks to its target one openat at a time, and opening a directory
// needs read permission on it. Android grants the shell user traverse-only
// access to the directories on the way to anywhere useful:
//
//	/data       drwxrwx--x system system
//	/data/local drwxr-x--x root   root
//	/data/local/tmp drwxrwx--x shell shell   <- writable, but unreachable from /
//
// So rooting at "/" makes every transfer fail with "openat data: permission
// denied" even though the destination itself is perfectly writable. Rooting at
// the target's own directory reaches it in one open, which the kernel resolves
// using traverse permission alone.
//
// This does not widen anything: the returned root is narrower than the volume,
// and it is exactly what a one-shot file approval already binds to
// (agent.gateFile). The symlink and non-regular-file refusals downstream are
// unchanged, so a symlink at the destination is still rejected rather than
// followed.
func unconstrainedRoot(target string) string { return unconstrainedRootFor(runtime.GOOS, target) }

func unconstrainedRootFor(goos, target string) string {
	if goos == "android" {
		return filepath.Dir(target)
	}
	return filepath.VolumeName(target) + string(filepath.Separator)
}
