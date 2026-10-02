//go:build windows

package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// These tests need real Windows processes and file locks, so CI's Linux job
// does not run them; run them on a Windows machine, the second one from an
// elevated shell.

// TestHelperProcessWaits is not a test: it is the long-running process the
// tests below start, so that a file is a running image or a pid is alive.
func TestHelperProcessWaits(t *testing.T) {
	if os.Getenv("WANCTL_TEST_HELPER_WAIT") != "1" {
		t.Skip("helper process only")
	}
	time.Sleep(2 * time.Minute)
	os.Exit(0)
}

func startWaitingHelper(t *testing.T, image string, attr *syscall.SysProcAttr) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(image, "-test.run=^TestHelperProcessWaits$")
	cmd.Env = append(os.Environ(), "WANCTL_TEST_HELPER_WAIT=1")
	cmd.SysProcAttr = attr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// A `service install` supervisor keeps running the image the previous update
// moved to wanctl.exe.old. The next update must still go through, and must
// leave that running file alone.
func TestReplaceBinaryWhenTheOldBackupIsARunningImage(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "wanctl.exe")
	copyFile(t, os.Args[0], dst+".old")
	startWaitingHelper(t, dst+".old", nil)

	if err := os.WriteFile(dst, []byte("current"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "wanctl-update-1.tmp")
	if err := os.WriteFile(src, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := replaceBinary(src, dst); err != nil {
		t.Fatalf("second update with a running .old: %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "new" {
		t.Fatalf("%s holds %q, want the new binary", dst, got)
	}
	if _, err := os.Stat(dst + ".old"); err != nil {
		t.Fatalf("the running .old must be left in place: %v", err)
	}
	backups := backupsOf(t, dst)
	if len(backups) != 1 {
		t.Fatalf("want exactly one timestamped backup, got %v", backups)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, backups[0])); string(got) != "current" {
		t.Fatalf("backup %s holds %q, want the replaced binary", backups[0], got)
	}

	// The backup of a binary that is no longer running goes at the next start.
	removeReplacedBinaries(dst)
	if left := backupsOf(t, dst); len(left) != 0 {
		t.Fatalf("stopped backups must be removed, left %v", left)
	}
	if _, err := os.Stat(dst + ".old"); err != nil {
		t.Fatalf("a running .old must survive the cleanup: %v", err)
	}
}

func backupsOf(t *testing.T, dst string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(dst))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), filepath.Base(dst)+".old-") {
			names = append(names, e.Name())
		}
	}
	return names
}

// An agent run as SYSTEM by a Scheduled Task grants administrators no
// terminate access; only SeDebugPrivilege opens it. The helper here gets a
// DACL that does the same to the user running the test, and the privilege is
// switched off first, as it is in an elevated console.
func TestTerminatePIDKillsAnAgentOnlyTheDebugPrivilegeOpens(t *testing.T) {
	if !setDebugPrivilege(t, false) {
		t.Skip("needs an elevated administrator (SeDebugPrivilege)")
	}
	t.Cleanup(func() { setDebugPrivilege(t, true) })

	// Everyone may wait on and query the process, nothing more.
	sd, err := windows.SecurityDescriptorFromString("D:(A;;0x00101000;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	attr := &syscall.SysProcAttr{ProcessAttributes: &syscall.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(syscall.SecurityAttributes{})),
		SecurityDescriptor: uintptr(unsafe.Pointer(sd)),
	}}
	cmd := startWaitingHelper(t, os.Args[0], attr)
	pid := cmd.Process.Pid

	if !canTerminatePID(pid) {
		t.Fatal("canTerminatePID: an administrator must be allowed to stop the agent")
	}
	if err := terminatePID(pid); err != nil {
		t.Fatalf("terminatePID: %v", err)
	}
	for i := 0; processAlive(pid); i++ {
		if i == 50 {
			t.Fatal("the agent is still running after terminatePID")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// setDebugPrivilege turns SeDebugPrivilege on or off for this process and
// reports whether the token holds it at all.
func setDebugPrivilege(t *testing.T, on bool) bool {
	t.Helper()
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &token); err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, windows.StringToUTF16Ptr("SeDebugPrivilege"), &luid); err != nil {
		t.Fatal(err)
	}
	privs := windows.Tokenprivileges{PrivilegeCount: 1}
	privs.Privileges[0] = windows.LUIDAndAttributes{Luid: luid}
	if on {
		privs.Privileges[0].Attributes = windows.SE_PRIVILEGE_ENABLED
	}
	// AdjustTokenPrivileges succeeds for a privilege the token lacks and says
	// so only through ERROR_NOT_ALL_ASSIGNED, which the x/sys wrapper drops.
	r1, _, lastErr := adjustTokenPrivileges.Call(uintptr(token), 0, uintptr(unsafe.Pointer(&privs)), 0, 0, 0)
	return r1 != 0 && lastErr != windows.ERROR_NOT_ALL_ASSIGNED
}

var adjustTokenPrivileges = windows.NewLazySystemDLL("advapi32.dll").NewProc("AdjustTokenPrivileges")
