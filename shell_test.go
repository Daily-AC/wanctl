package main

import (
	"bytes"
	"strings"
	"testing"

	"wanctl/internal/client"
	"wanctl/internal/policy"
)

// What the device sends for one command, chunked the way it may arrive, and
// what the prompt-side terminal should end up showing.
func TestShellOutputDropsOnlyTheMarkerNewline(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []string
		want   string
	}{
		{"posix line", []string{"a\n\n"}, "a\n"},
		{"posix line split before the marker newline", []string{"a\n", "\n"}, "a\n"},
		{"posix blank line the command printed", []string{"a\n\n\n"}, "a\n\n"},
		{"posix no trailing newline", []string{"a\n"}, "a\n"},
		{"posix no output", []string{"\n"}, ""},
		{"posix several pages", []string{"one\ntw", "o\n", "\n"}, "one\ntwo\n"},
		{"powershell line", []string{"C:\\\r\n"}, "C:\\\r\n"},
		{"powershell split at the line end", []string{"x\r", "\n"}, "x\r\n"},
		{"powershell no output", nil, ""},
	} {
		var got bytes.Buffer
		o := &shellOutput{w: &got}
		for _, c := range tc.chunks {
			if n, err := o.Write([]byte(c)); err != nil || n != len(c) {
				t.Fatalf("%s: Write(%q) = %d, %v", tc.name, c, n, err)
			}
		}
		o.end()
		if got.String() != tc.want {
			t.Errorf("%s: shown %q, want %q", tc.name, got.String(), tc.want)
		}
	}
}

func TestShellPromptShortensHome(t *testing.T) {
	for _, tc := range []struct{ cwd, home, want string }{
		{"/home/ann", "/home/ann", "box:~$ "},
		{"/home/ann/src", "/home/ann", "box:~/src$ "},
		{"/home/annex", "/home/ann", "box:/home/annex$ "},
		{`C:\Users\ann\Desktop`, `C:\Users\ann`, `box:~\Desktop$ `},
		{"/tmp", "/", "box:/tmp$ "},         // a root home would make every path ~
		{`C:\x`, `C:\`, `box:C:\x$ `},       // likewise a drive root
		{"", "", "box$ "},                   // an agent that does not report its directory
		{"/srv/a\nb", "", "box:/srv/a?b$ "}, // liner refuses control characters outright
	} {
		s := &remoteShell{name: "box", cwd: tc.cwd, home: tc.home}
		if got := s.prompt(); got != tc.want {
			t.Errorf("cwd %q home %q: prompt %q, want %q", tc.cwd, tc.home, got, tc.want)
		}
	}
}

// Colored like a default ssh login: bold green where user@host goes, bold
// blue for the directory. Text from the device is made safe before it is
// wrapped, so a device cannot close or restyle those colors itself.
func TestShellPromptIsColoredLikeSSH(t *testing.T) {
	for _, tc := range []struct{ name, cwd, want string }{
		{"5090大机子", "/srv/数据", "\x1b[01;32m5090大机子\x1b[00m:\x1b[01;34m/srv/数据\x1b[00m$ "},
		{"box", "", "\x1b[01;32mbox\x1b[00m$ "},
		{"box\x1b[31m", "/tmp\x1b]0;owned\x07", "\x1b[01;32mbox?[31m\x1b[00m:\x1b[01;34m/tmp?]0;owned?\x1b[00m$ "},
	} {
		s := &remoteShell{name: tc.name, cwd: tc.cwd, color: true}
		if got := s.prompt(); got != tc.want {
			t.Errorf("prompt %q, want %q", got, tc.want)
		}
	}
}

func TestShellColorFollowsNoColorAndDumbTerminals(t *testing.T) {
	for _, tc := range []struct {
		noColor, term string
		want          bool
	}{
		{"", "xterm-256color", true},
		{"1", "xterm-256color", false},
		{"", "dumb", false},
	} {
		t.Setenv("NO_COLOR", tc.noColor)
		t.Setenv("TERM", tc.term)
		if got := shellColor(); got != tc.want {
			t.Errorf("NO_COLOR=%q TERM=%q: color %v, want %v", tc.noColor, tc.term, got, tc.want)
		}
	}
}

func TestShellExitWords(t *testing.T) {
	for line, want := range map[string]bool{
		"exit": true, "exit 3": true, "logout": true,
		"exit; ls": false, "exitcode": false, "echo exit": false,
	} {
		if got := isShellExit(line); got != want {
			t.Errorf("isShellExit(%q) = %v, want %v", line, got, want)
		}
	}
}

func TestShellRefusesADeviceThatWouldAskEveryLine(t *testing.T) {
	if err := shellNeedsBypass("box", client.AgentStatus{Detailed: true, Mode: policy.ModeBypass}, ""); err != nil {
		t.Fatalf("bypass device refused: %v", err)
	}
	err := shellNeedsBypass("box", client.AgentStatus{Detailed: true, Mode: policy.ModeNormal}, "https://portal.example")
	if err == nil {
		t.Fatal("normal-mode device accepted")
	}
	for _, want := range []string{"box is in normal mode", "bypass", "https://portal.example", "wanctl exec"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not say %q:\n%s", want, err)
		}
	}
	// An agent that predates the status verb cannot say which mode it is in.
	if err := shellNeedsBypass("box", client.AgentStatus{}, ""); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Errorf("old agent: %v", err)
	}
}

func TestShellReplacesAFullShellBeforeTheNextLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    *remoteShell
		want bool
	}{
		{"one command short of the ledger", &remoteShell{commands: shellMaxCommands - 1}, false},
		{"ledger full", &remoteShell{commands: shellMaxCommands}, true},
		{"half the output used", &remoteShell{output: shellMaxOutput / 2}, true},
		{"command text full", &remoteShell{input: shellMaxInput}, true},
		{"shell gone", &remoteShell{stale: true}, true},
	} {
		if got := tc.s.replaceReason("ls") != ""; got != tc.want {
			t.Errorf("%s: replace = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestShellPromptNamesTheDeviceAsTyped(t *testing.T) {
	for _, tc := range []struct{ target, device, want string }{
		{"5090大机子", "zyl", "5090大机子"},
		{"team/lab-box", "lab-host", "lab-box"},
		{"59619330-df32-4373-acae-fc344a930eab", "zyl", "zyl"},
		{"team/59619330-df32-4373-acae-fc344a930eab", "zyl", "zyl"},
		{"59619330-df32-4373-acae-fc344a930eab", "", "59619330-df32-4373-acae-fc344a930eab"},
	} {
		if got := shellName(tc.target, tc.device); got != tc.want {
			t.Errorf("shellName(%q, %q) = %q, want %q", tc.target, tc.device, got, tc.want)
		}
	}
}
