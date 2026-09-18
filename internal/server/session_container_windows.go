package server

import (
	"errors"
	"fmt"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// sessionContainer is a job object holding the session's PowerShell. A job
// object is the only thing on Windows that names "this process and everything
// it will ever start" — a pid/ppid walk cannot, because a parent pid is just a
// number that outlives its owner and gets reused.
//
// The job is created without JOB_OBJECT_LIMIT_BREAKAWAY_OK or
// SILENT_BREAKAWAY_OK, so no descendant can leave it.
type sessionContainer struct{ job windows.Handle }

// prepareSessionContainer has nothing to do before the process starts: a job is
// created and the process assigned to it afterwards.
func prepareSessionContainer(cmd *exec.Cmd) {}

// captureSessionContainer puts the started shell into a fresh job object.
//
// There is a short window between cmd.Start and the assignment. Go does not
// expose the thread handle a CREATE_SUSPENDED start would need to resume, and
// powershell.exe has not finished loading the CLR in that window, let alone
// forked anything, so nothing can escape through it in practice. Windows 8 and
// later allow nested jobs, so this works even when the agent itself already
// runs inside one (a service host, a container).
func captureSessionContainer(cmd *exec.Cmd) (*sessionContainer, error) {
	if cmd.Process == nil {
		return nil, errors.New("session shell has not started")
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create session job object: %w", err)
	}
	// KILL_ON_JOB_CLOSE ties the session's lifetime to this handle: if the
	// agent exits without closing sessions, the kernel still ends them.
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("configure session job object: %w", err)
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("open session shell %d: %w", cmd.Process.Pid, err)
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("assign session shell to job object: %w", err)
	}
	return &sessionContainer{job: job}, nil
}

// Kill ends every process in the job in one call.
func (c *sessionContainer) Kill() error {
	if c == nil || c.job == 0 {
		return nil
	}
	// Every error here is propagated. The job is one this process created and
	// still holds, and terminating a job whose processes have all exited
	// succeeds, so there is no benign failure to forgive — and reporting a
	// clean cancellation for a kill that did not happen is the one answer that
	// is never true.
	if err := windows.TerminateJobObject(c.job, 1); err != nil {
		return fmt.Errorf("terminate session job object: %w", err)
	}
	return nil
}

// Close releases the job handle, which also ends anything still in it.
func (c *sessionContainer) Close() error {
	if c == nil || c.job == 0 {
		return nil
	}
	handle := c.job
	c.job = 0
	return windows.CloseHandle(handle)
}
