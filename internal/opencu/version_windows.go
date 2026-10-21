//go:build windows

package opencu

import (
	"golang.org/x/sys/windows"
	"os"
)

func versionManifestPath() string {
	var processSession uint32
	if windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &processSession) != nil {
		return ""
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return ""
	}
	systemZero := processSession == 0 && user.User.Sid.IsWellKnown(windows.WinLocalSystemSid)
	if !systemZero {
		return selectVersionManifestPath(false, os.Getenv("LOCALAPPDATA"), 0, nil)
	}
	session := windows.WTSGetActiveConsoleSessionId()
	path := selectVersionManifestPath(true, "", session, func(id uint32) (string, error) {
		var token windows.Token
		if err := windows.WTSQueryUserToken(id, &token); err != nil {
			return "", err
		}
		defer token.Close()
		return token.GetUserProfileDirectory()
	})
	if windows.WTSGetActiveConsoleSessionId() != session {
		return ""
	}
	return path
}
