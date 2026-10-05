//go:build windows

package desktop

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const helperPipePrefix = `\\.\pipe\wanctl-desktop-`

// Cross-session handle inheritance is forbidden by Windows. Both pipe servers
// are created before the user process, accept only this logon and SYSTEM, reject
// remote clients, and verify the connecting PID before sending any job data.
func startConsoleHelper(ctx context.Context, token windows.Token, session uint32, logon string) (_ *helperProcess, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	base := helperPipePrefix + NewID()
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;SY)(A;;GRGW;;;" + logon + ")")
	if err != nil {
		return nil, helperStartError("pipe security", err)
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	makePipe := func(name string, direction uint32) (windows.Handle, error) {
		return windows.CreateNamedPipe(utf16ptr(name), direction|windows.FILE_FLAG_OVERLAPPED|windows.FILE_FLAG_FIRST_PIPE_INSTANCE,
			windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS, 1, 65536, 65536, 0, &sa)
	}
	in, err := makePipe(base+"-in", windows.PIPE_ACCESS_OUTBOUND)
	if err != nil {
		return nil, helperStartError("stdin pipe", err)
	}
	defer func() {
		if in != windows.InvalidHandle {
			windows.CloseHandle(in)
		}
	}()
	out, err := makePipe(base+"-out", windows.PIPE_ACCESS_INBOUND)
	if err != nil {
		return nil, helperStartError("stdout pipe", err)
	}
	defer func() {
		if out != windows.InvalidHandle {
			windows.CloseHandle(out)
		}
	}()
	var env *uint16
	if err := windows.CreateEnvironmentBlock(&env, token, false); err != nil {
		return nil, helperStartError("user environment", err)
	}
	defer windows.DestroyEnvironmentBlock(env)
	dir, err := token.GetUserProfileDirectory()
	if err != nil {
		return nil, helperStartError("user profile directory", err)
	}
	path, err := os.Executable()
	if err != nil {
		return nil, helperStartError("executable", err)
	}
	command := windows.ComposeCommandLine([]string{path, "__desktop-pipe", base})
	si := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{})), Desktop: utf16ptr(`winsta0\default`)}
	var pi windows.ProcessInformation
	// No inherited SYSTEM handles, token/environment, or working directory.
	if err := windows.CreateProcessAsUser(token, utf16ptr(path), utf16ptr(command), nil, nil, false,
		windows.CREATE_NO_WINDOW|windows.CREATE_UNICODE_ENVIRONMENT, env, utf16ptr(dir), &si, &pi); err != nil {
		return nil, helperStartError("CreateProcessAsUser", err)
	}
	RecordSession(ctx, session)
	defer windows.CloseHandle(pi.Thread)
	defer windows.CloseHandle(pi.Process)
	ready := false
	defer func() {
		if !ready {
			windows.TerminateProcess(pi.Process, 1)
			windows.WaitForSingleObject(pi.Process, 2000)
		}
	}()
	for _, pipe := range []windows.Handle{in, out} {
		if err := connectHelperPipe(ctx, pipe, pi.Process, pi.ProcessId); err != nil {
			return nil, helperStartError("pipe connection", err)
		}
	}
	process, err := os.FindProcess(int(pi.ProcessId))
	if err != nil {
		return nil, helperStartError("process handle", err)
	}
	stdin, stdout := os.NewFile(uintptr(in), "desktop-stdin"), os.NewFile(uintptr(out), "desktop-stdout")
	in, out = windows.InvalidHandle, windows.InvalidHandle // os.File now owns them
	ready = true
	return &helperProcess{stdin: stdin, stdout: stdout, kill: process.Kill, wait: func() error {
		state, err := process.Wait()
		if err == nil && !state.Success() {
			err = errors.New("desktop helper exited unsuccessfully")
		}
		return err
	}}, nil
}

func connectHelperPipe(ctx context.Context, pipe, process windows.Handle, pid uint32) error {
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(event)
	overlap := windows.Overlapped{HEvent: event}
	err = windows.ConnectNamedPipe(pipe, &overlap)
	if err == windows.ERROR_IO_PENDING {
		// The OVERLAPPED and event must survive cancellation until the operation
		// completes. Never close a pipe with a pending stack-backed operation.
		defer func() {
			windows.CancelIoEx(pipe, &overlap)
			var n uint32
			windows.GetOverlappedResult(pipe, &overlap, &n, true)
		}()
		for {
			if err = ctx.Err(); err != nil {
				return err
			}
			which, e := windows.WaitForMultipleObjects([]windows.Handle{event, process}, false, 20)
			if e != nil {
				return e
			}
			if which == windows.WAIT_OBJECT_0 {
				break
			}
			if which == windows.WAIT_OBJECT_0+1 {
				return errors.New("helper exited before connecting")
			}
			if which != uint32(windows.WAIT_TIMEOUT) {
				return errors.New("pipe wait failed")
			}
		}
		var n uint32
		if err = windows.GetOverlappedResult(pipe, &overlap, &n, false); err != nil {
			return err
		}
	} else if err != nil && err != windows.ERROR_PIPE_CONNECTED {
		return err
	}
	var client uint32
	if err := windows.GetNamedPipeClientProcessId(pipe, &client); err != nil {
		return err
	}
	if client != pid {
		return errors.New("unexpected desktop pipe client")
	}
	return nil
}

// PipeHelperMain only attaches streams; HelperMain's framing, EOF cancellation,
// per-helper input marker, banner, focus and foreign-input rules are unchanged.
func PipeHelperMain(base string) int {
	if !strings.HasPrefix(base, helperPipePrefix) || len(base) != len(helperPipePrefix)+32 || !validID(strings.TrimPrefix(base, helperPipePrefix)) {
		return 1
	}
	open := func(name string, access uint32) (*os.File, error) {
		h, err := windows.CreateFile(utf16ptr(name), access, 0, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_OVERLAPPED|windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(h), "desktop-pipe"), nil
	}
	in, err := open(base+"-in", windows.GENERIC_READ)
	if err != nil {
		return 1
	}
	defer in.Close()
	out, err := open(base+"-out", windows.GENERIC_WRITE)
	if err != nil {
		return 1
	}
	defer out.Close()
	return HelperMain(in, out)
}
