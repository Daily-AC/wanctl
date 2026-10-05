//go:build windows

package desktop

import (
	"context"
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
	"wanctl/internal/protocol"
)

func processSession() (uint32, error) {
	var id uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &id); err != nil {
		return 0, errors.New("cannot determine desktop session")
	}
	return id, nil
}

// SessionZero selects the legacy capture route without probing another user
// when this agent already runs in a desktop session.
func SessionZero() (bool, error) {
	id, err := processSession()
	return id == 0, err
}

func runPlatformHelper(ctx context.Context, job Job) (protocol.DesktopResult, []byte, error) {
	return runSessionHelper(ctx, job, sessionCalls{processSession, windows.WTSGetActiveConsoleSessionId, queryConsoleUser}, startInheritedHelper)
}

func tokenUint32(token windows.Token, class uint32) (uint32, error) {
	var value, size uint32
	err := windows.GetTokenInformation(token, class, (*byte)(unsafe.Pointer(&value)), uint32(unsafe.Sizeof(value)), &size)
	return value, err
}

func queryConsoleUser(session uint32) (consoleUser, error) {
	var none consoleUser
	var token windows.Token
	if err := windows.WTSQueryUserToken(session, &token); err != nil {
		if errors.Is(err, windows.ERROR_NO_TOKEN) {
			return none, errNoConsoleUser
		}
		return none, errors.New("desktop unavailable: cannot obtain the console user's token; session-0 agent must run as LocalSystem")
	}
	defer func() { token.Close() }()
	// Never turn a filtered user token into its elevated linked token. If WTS
	// supplied a full UAC token, explicitly select and verify its limited peer.
	elevation, err := tokenUint32(token, windows.TokenElevationType)
	if err != nil {
		return none, errors.New("cannot check console user token rights")
	}
	if elevation == 2 { // TokenElevationTypeFull
		limited, err := token.GetLinkedToken()
		if err != nil {
			return none, errors.New("cannot obtain the console user's limited token")
		}
		token.Close()
		token = limited
		kind, err := tokenUint32(token, windows.TokenElevationType)
		if err != nil || kind != 3 { // TokenElevationTypeLimited
			return none, errors.New("console user's linked token is not limited")
		}
	}
	id, err := tokenUint32(token, windows.TokenSessionId)
	if err != nil || id != session {
		return none, errors.New("console user token belongs to a different session")
	}
	groups, err := token.GetTokenGroups()
	if err != nil {
		return none, errors.New("cannot identify console user logon")
	}
	var logon string
	for _, group := range groups.AllGroups() {
		if group.Attributes&windows.SE_GROUP_LOGON_ID == windows.SE_GROUP_LOGON_ID {
			logon = group.Sid.String()
			break
		}
	}
	if logon == "" {
		return none, errors.New("console user token has no interactive logon SID")
	}
	var primary windows.Token
	if err := windows.DuplicateTokenEx(token, windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE|windows.TOKEN_ASSIGN_PRIMARY, nil, windows.SecurityImpersonation, windows.TokenPrimary, &primary); err != nil {
		return none, errors.New("cannot prepare console user primary token")
	}
	return consoleUser{
		logon: logon,
		close: func() { primary.Close() },
		start: func(ctx context.Context) (*helperProcess, error) {
			return startConsoleHelper(ctx, primary, session, logon)
		},
	}, nil
}

func helperStartError(operation string, err error) error {
	// Operation and Win32 errno only: no path, environment or private job data.
	var errno windows.Errno
	if errors.As(err, &errno) {
		return fmt.Errorf("desktop helper %s failed (Windows error %d)", operation, uint32(errno))
	}
	return fmt.Errorf("desktop helper %s failed", operation)
}
