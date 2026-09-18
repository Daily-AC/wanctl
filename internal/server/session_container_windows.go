package server

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
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
type sessionContainer struct {
	mu     sync.Mutex
	job    windows.Handle
	killed bool
}

// prepareSessionContainer starts the shell suspended. Assigning a process to a
// job after it is already running is a race the job cannot win: a shell that
// forks a worker before the assignment lands leaves that worker outside the
// job forever, because a job captures descendants created *after* a process
// joins it. Creating suspended and resuming only once the assignment has
// succeeded removes the window instead of arguing about how small it is.
func prepareSessionContainer(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
}

// captureSessionContainer puts the suspended shell into a fresh job object and
// then lets it run. Every failure returns an error and leaves the process
// suspended for the caller to kill, so a shell that could not be contained
// never executes a single instruction.
//
// Windows 8 and later allow nested jobs, so this works when the agent already
// runs inside one — but not under every ancestor configuration, and a refusal
// here is reported rather than quietly downgraded to an uncontained session.
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
	if err := resumeProcess(cmd.Process.Pid); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	return &sessionContainer{job: job}, nil
}

// resumeProcess starts a process created with CREATE_SUSPENDED. Go does not
// hand back the thread handle CreateProcess returned, so the process's threads
// are found through a toolhelp snapshot. A freshly created suspended process
// has exactly one.
func resumeProcess(pid int) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("snapshot threads of session shell %d: %w", pid, err)
	}
	defer windows.CloseHandle(snapshot)

	var entry windows.ThreadEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	resumed := 0
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != uint32(pid) {
			continue
		}
		thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if openErr != nil {
			return fmt.Errorf("open thread %d of session shell %d: %w", entry.ThreadID, pid, openErr)
		}
		_, resumeErr := windows.ResumeThread(thread)
		windows.CloseHandle(thread)
		if resumeErr != nil {
			return fmt.Errorf("resume thread %d of session shell %d: %w", entry.ThreadID, pid, resumeErr)
		}
		resumed++
	}
	if err != nil && !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return fmt.Errorf("walk threads of session shell %d: %w", pid, err)
	}
	if resumed == 0 {
		return fmt.Errorf("session shell %d has no thread to resume", pid)
	}
	return nil
}

// Kill ends every process in the job in one call. It is a no-op after the first
// call: a job handle names the job unambiguously, so repeating the call is safe
// on Windows, but one kill per container keeps both platforms to the same rule.
func (c *sessionContainer) Kill() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.job == 0 || c.killed {
		return nil
	}
	c.killed = true
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

// reap exists for symmetry with the Unix container, where a reaped pid stops
// naming the group. A job handle keeps naming its job until it is closed, so
// there is nothing to invalidate here.
func (c *sessionContainer) reap() {}

// Close releases the job handle, which also ends anything still in it.
func (c *sessionContainer) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.job == 0 {
		return nil
	}
	handle := c.job
	c.job = 0
	return windows.CloseHandle(handle)
}
