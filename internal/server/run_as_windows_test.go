package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"wanctl/internal/script"
)

// Opt in from a SYSTEM shell on a Windows machine with this user logged on.
// Cross-compiling these tests is not evidence that token execution works.
func TestRunAsLoggedOnUser(t *testing.T) {
	username := os.Getenv("WANCTL_TEST_AS_USER")
	if username == "" {
		t.Skip("set WANCTL_TEST_AS_USER and run as SYSTEM on Windows")
	}
	token, err := loggedOnUserToken(username)
	if err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	profile, err := token.GetUserProfileDirectory()
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("wanctl-as-test-%d", time.Now().UnixNano())
	regPath := sid + `\Software\` + name
	t.Cleanup(func() { registry.DeleteKey(registry.USERS, regPath) })
	source := fmt.Sprintf(`$ErrorActionPreference='Stop'
$dir=Join-Path $env:LOCALAPPDATA '%s'
New-Item -ItemType Directory -Path $dir | Out-Null
$file=Join-Path $dir 'owned.txt'
[IO.File]::WriteAllText($file, 'user-owned')
$key=New-Item 'HKCU:\Software\%s' -Force
New-ItemProperty -Path $key.PSPath -Name 'Marker' -Value 'user-hive' | Out-Null
@{
  SID=[Security.Principal.WindowsIdentity]::GetCurrent().User.Value
  Profile=$env:USERPROFILE; LocalAppData=$env:LOCALAPPDATA; Temp=$env:TEMP
  Cwd=(Get-Location).Path; File=$file
  Owner=(Get-Acl $file).GetOwner([Security.Principal.SecurityIdentifier]).Value
  Text='中文 café'
} | ConvertTo-Json -Compress
`, name, name)
	command, err := script.Command(script.PowerShell, []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var out bytes.Buffer
	code, err := RunAsUserContext(ctx, "", command, "", username, &out)
	if err != nil || code != 0 {
		t.Fatalf("run as user: exit %d, %v\n%s", code, err, out.String())
	}
	var got struct{ SID, Profile, LocalAppData, Temp, Cwd, File, Owner, Text string }
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &got); err != nil {
		t.Fatalf("decode output: %v\n%s", err, out.String())
	}
	if filepath.Base(got.File) != "owned.txt" || filepath.Base(filepath.Dir(got.File)) != name {
		t.Fatalf("unexpected test file path: %q", got.File)
	}
	t.Cleanup(func() { os.RemoveAll(filepath.Dir(got.File)) })
	// An administrator with UAC disabled can have Administrators as the
	// default owner. What matters is the user's identity, no SYSTEM ownership,
	// and the separate user process below being able to update/delete the file.
	if got.SID != sid || got.Owner == "" || got.Owner == "S-1-5-18" {
		t.Errorf("identity/owner: got %+v, want user SID %s and a non-SYSTEM owner", got, sid)
	}
	if !strings.EqualFold(got.Profile, profile) || !strings.EqualFold(got.Cwd, profile) {
		t.Errorf("profile/cwd: got %+v, want %s", got, profile)
	}
	if got.LocalAppData == "" || got.Temp == "" || strings.EqualFold(got.Temp, filepath.Join(os.Getenv("SystemRoot"), "Temp")) {
		t.Errorf("user environment: %+v", got)
	}
	if got.Text != "中文 café" {
		t.Errorf("script/output encoding: %q", got.Text)
	}
	key, err := registry.OpenKey(registry.USERS, regPath, registry.QUERY_VALUE)
	if err != nil {
		t.Fatalf("child did not write HKCU in the user's hive: %v", err)
	}
	marker, _, err := key.GetStringValue("Marker")
	key.Close()
	if err != nil || marker != "user-hive" {
		t.Fatalf("HKCU marker = %q, %v", marker, err)
	}
	// A separate user process must be able to update and remove what the
	// installer wrote. cwd is passed outside the shell source.
	out.Reset()
	code, err = RunAsUserContext(ctx, "", `[IO.File]::AppendAllText((Join-Path (Get-Location) 'owned.txt'), '-updated'); Remove-Item -LiteralPath 'owned.txt'; exit 7`, filepath.Dir(got.File), username, &out)
	if err != nil || code != 7 {
		t.Fatalf("user update/delete and exit code: %d, %v\n%s", code, err, out.String())
	}
	if _, err := os.Stat(got.File); !os.IsNotExist(err) {
		t.Fatalf("user could not delete its file: %v", err)
	}
	if _, err := RunAsUserContext(ctx, "", "whoami", "", name+"-missing", &out); err == nil {
		t.Fatal("unknown user unexpectedly ran a command")
	}
}

func TestRunAsRequiresSystem(t *testing.T) {
	caller, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if caller.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		t.Skip("requires a non-SYSTEM test runner")
	}
	var out bytes.Buffer
	_, err = RunAsUserContext(context.Background(), "", "whoami", "", "unused", &out)
	if err == nil || !strings.Contains(err.Error(), "run as SYSTEM") || out.Len() != 0 {
		t.Fatalf("non-SYSTEM refusal: %v, output %q", err, out.String())
	}
}

func TestRunAsCancelledBeforeStart(t *testing.T) {
	username := os.Getenv("WANCTL_TEST_AS_USER")
	if username == "" {
		t.Skip("set WANCTL_TEST_AS_USER and run as SYSTEM on Windows")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	_, err := RunAsUserContext(ctx, "", "Write-Output should-not-run", "", username, &out)
	if !errors.Is(err, context.Canceled) || out.Len() != 0 {
		t.Fatalf("cancelled --as: %v, output %q", err, out.String())
	}
}
