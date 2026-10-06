package desktop

import (
	"errors"
	"unsafe"
)

// ErrLocked is shared by helper results and the legacy capture path.
var ErrLocked = errors.New("desktop unavailable: screen locked or secure desktop active")

const (
	wtsSessionUnknown  int32 = -1
	wtsSessionLocked   int32 = 0
	wtsSessionUnlocked int32 = 1
)

// Only the prefix we read from WTSINFOEXW. Data's full C union contains
// LARGE_INTEGERs and is 8-byte aligned on amd64/arm64, despite this prefix
// containing only DWORD/LONG fields. Do not drop the padding after Level.
type wtsInfoExPrefix struct {
	Level uint32
	_     uint32
	Data  struct {
		SessionID    uint32
		SessionState int32
		SessionFlags int32
	}
}

func wtsSessionFlags(info *wtsInfoExPrefix, size uint32, session uint32) int32 {
	if info == nil || uintptr(size) < unsafe.Sizeof(*info) || info.Level != 1 || info.Data.SessionID != session {
		return wtsSessionUnknown
	}
	return info.Data.SessionFlags
}

func checkSessionDesktop(session uint32, query func(uint32) int32, checkDesktop func() error) error {
	// LockApp can remain on Default. Unknown (including failed WTS queries)
	// must retain the input-desktop check, not imply an unlocked desktop.
	if query(session) == wtsSessionLocked {
		return ErrLocked
	}
	return checkDesktop()
}
