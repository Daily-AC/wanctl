package server

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processParents snapshots the process table as a pid -> parent-pid map using
// the toolhelp snapshot API, which needs no cgo, forks nothing and flashes no
// console window (the agent runs as a service, so a console flash is visible to
// whoever is at the machine).
func processParents() (map[int]int, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("snapshot process table: %w", err)
	}
	defer windows.CloseHandle(snapshot)

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	parents := map[int]int{}
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		parents[int(entry.ProcessID)] = int(entry.ParentProcessID)
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) && err != nil {
		return nil, fmt.Errorf("walk process table: %w", err)
	}
	if len(parents) == 0 {
		return nil, errors.New("snapshot process table: no readable rows")
	}
	return parents, nil
}

// terminateProcess ends one process. A non-interactive PowerShell child has no
// message loop, so there is nothing gentler than TerminateProcess to ask.
func terminateProcess(pid int) error {
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			// Gone between the snapshot and the open, or not ours to kill.
			return nil
		}
		return err
	}
	defer windows.CloseHandle(handle)
	if err := windows.TerminateProcess(handle, 1); err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil // it exited while we held the handle
		}
		return err
	}
	return nil
}
