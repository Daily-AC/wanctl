package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// RunAsUserContext borrows a logged-on user's identity, not their persistent
// shell. No password, scheduled task, or script in SYSTEM's temp directory is
// involved. The normal exec policy still applies to the command.
func RunAsUserContext(ctx context.Context, shell, command, cwd, username string, out io.Writer) (int, error) {
	token, err := loggedOnUserToken(username)
	if err != nil {
		return -1, err
	}
	defer token.Close()

	user, err := token.GetTokenUser()
	if err != nil {
		return -1, fmt.Errorf("read user token: %w", err)
	}
	// WTS supplies an existing interactive logon, whose profile is already
	// loaded. Refuse an unloaded hive (for example during logoff) rather than
	// letting HKCU resolve to a default profile. Keep it open while the child runs.
	hive, err := registry.OpenKey(registry.USERS, user.User.Sid.String(), registry.READ)
	if err != nil {
		return -1, fmt.Errorf("profile for %q is not loaded; keep the user logged on: %w", username, err)
	}
	defer hive.Close()
	env, err := token.Environ(false) // CreateEnvironmentBlock, without SYSTEM's environment
	if err != nil {
		return -1, fmt.Errorf("create environment for %q: %w", username, err)
	}
	if cwd == "" {
		cwd, err = token.GetUserProfileDirectory()
		if err != nil {
			return -1, fmt.Errorf("get profile directory for %q: %w", username, err)
		}
	}

	// Windows forbids inheriting pipe handles across Terminal Services
	// sessions. Run this noninteractive child in the agent's session, using a
	// private copy of the user's primary token. The logon identity, HKCU and
	// environment remain the user's; their interactive token is never changed.
	// https://learn.microsoft.com/windows/win32/api/processthreadsapi/nf-processthreadsapi-createprocessasuserw
	var sessionID uint32
	if err := windows.ProcessIdToSessionId(uint32(os.Getpid()), &sessionID); err != nil {
		return -1, fmt.Errorf("get agent session: %w", err)
	}
	if err := windows.SetTokenInformation(token, windows.TokenSessionId, (*byte)(unsafe.Pointer(&sessionID)), uint32(unsafe.Sizeof(sessionID))); err != nil {
		return -1, fmt.Errorf("set child session: %w", err)
	}
	cmd := oneShotCommand(ctx, shell, command, cwd, out)
	cmd.Env = env
	// Go's Windows StartProcess calls CreateProcessAsUserW when Token is set,
	// retaining the existing hidden console, streaming and cancellation hooks.
	cmd.SysProcAttr.Token = syscall.Token(token)
	return runOneShotCommand(ctx, cmd)
}

func loggedOnUserToken(username string) (windows.Token, error) {
	caller, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return 0, fmt.Errorf("read agent identity: %w", err)
	}
	if !caller.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		return 0, fmt.Errorf("--as requires the Windows agent to run as SYSTEM")
	}
	// LookupAccountName resolves both bare local names and DOMAIN\user names
	// to a SID. Compare identities, not display names from different domains.
	if strings.HasPrefix(username, `.\`) {
		host, err := os.Hostname()
		if err != nil {
			return 0, err
		}
		username = host + username[1:]
	}
	want, _, _, err := windows.LookupSID("", username)
	if err != nil {
		return 0, fmt.Errorf("resolve --as user %q: %w", username, err)
	}
	var sessions *windows.WTS_SESSION_INFO
	var count uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &sessions, &count); err != nil {
		return 0, fmt.Errorf("enumerate logged-on sessions: %w", err)
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(sessions)))

	var selected windows.Token
	defer func() {
		if selected != 0 {
			selected.Close()
		}
	}()
	for _, session := range unsafe.Slice(sessions, count) {
		if session.SessionID == 0 || (session.State != windows.WTSActive && session.State != windows.WTSDisconnected) {
			continue
		}
		var token windows.Token
		if err := windows.WTSQueryUserToken(session.SessionID, &token); err != nil {
			if errors.Is(err, windows.ERROR_NO_TOKEN) {
				continue // a session without a logged-on user, or one logging off
			}
			return 0, fmt.Errorf("query user token for session %d: %w", session.SessionID, err)
		}
		user, err := token.GetTokenUser()
		if err != nil {
			token.Close()
			return 0, fmt.Errorf("read session %d identity: %w", session.SessionID, err)
		}
		if !windows.EqualSid(user.User.Sid, want) {
			token.Close()
			continue
		}
		if selected != 0 {
			token.Close()
			return 0, fmt.Errorf("user %q has multiple logged-on sessions; sign out of the unused sessions before using --as", username)
		}
		selected = token
	}
	if selected == 0 {
		return 0, fmt.Errorf("user %q has no logged-on interactive session; sign in before using --as", username)
	}
	// Use exactly the interactive token; never request its elevated UAC sibling.
	var primary windows.Token
	access := uint32(windows.TOKEN_QUERY | windows.TOKEN_DUPLICATE | windows.TOKEN_ASSIGN_PRIMARY | windows.TOKEN_ADJUST_SESSIONID)
	if err := windows.DuplicateTokenEx(selected, access, nil, windows.SecurityImpersonation, windows.TokenPrimary, &primary); err != nil {
		return 0, fmt.Errorf("duplicate user token for %q: %w", username, err)
	}
	return primary, nil
}
