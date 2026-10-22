//go:build windows

package desktop

import (
	"context"
	"errors"
	"golang.org/x/sys/windows"
	"os"
	"unsafe"
)

func startInstallHelper(ctx context.Context) (*helperProcess, error) {
	console := windows.WTSGetActiveConsoleSessionId()
	if console == 0 || console == ^uint32(0) {
		return nil, errNoConsoleUser
	}
	current, err := processSession()
	if err != nil {
		return nil, err
	}
	if current != 0 && current != console {
		return nil, errors.New("OpenCU installation requires the signed-in console user's session")
	}
	return startPlatformOpenCUHelper(ctx)
}
func checkInstallSession() error {
	id, err := processSession()
	if err != nil {
		return err
	}
	if id == 0 || id != windows.WTSGetActiveConsoleSessionId() {
		return errNoConsoleUser
	}
	token := windows.GetCurrentProcessToken()
	elevated, err := tokenUint32(token, windows.TokenElevation)
	if err != nil || elevated != 0 {
		return errors.New("OpenCU installation requires the console user's normal non-elevated token")
	}
	if os.Getenv("LOCALAPPDATA") == "" {
		return errors.New("console user's LOCALAPPDATA is unavailable")
	}
	// Contain the helper BEFORE it starts PowerShell. Children inherit this
	// job. Keep its non-inheritable handle until process exit: closing it here
	// would kill this helper before it could report the installer's result.
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return errors.New("cannot contain the OpenCU installer process tree")
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE}}
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return errors.New("cannot bound the OpenCU installer process tree")
	}
	if err = windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		windows.CloseHandle(job)
		return errors.New("cannot contain the OpenCU installer in this Windows session")
	}
	// The OS closes job when this short-lived helper exits or is killed.
	return nil
}
