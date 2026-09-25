package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"
)

// The logon task is what brings the agent back on Windows, and the schtasks
// defaults it used to get took a real device offline: the console window it
// opened at logon was closed by hand, which killed the agent. The same
// defaults also skip the start on battery and end the task after 72 hours.
func TestWinTaskXMLKeepsTheAgentHiddenAndRunning(t *testing.T) {
	const (
		self    = `C:\Users\alice\AppData\Local\wanctl\wanctl.exe`
		conhost = `C:\Windows\System32\conhost.exe`
	)
	doc := winTaskXML(`LAB\alice`, conhost, self, []string{"--name", `R&D "lab" box`})

	var task struct {
		TriggerUser string `xml:"Triggers>LogonTrigger>UserId"`
		Principal   struct {
			UserID    string `xml:"UserId"`
			LogonType string
			RunLevel  string
		} `xml:"Principals>Principal"`
		Settings struct {
			DisallowStartIfOnBatteries string
			StopIfGoingOnBatteries     string
			ExecutionTimeLimit         string
		}
		Command   string `xml:"Actions>Exec>Command"`
		Arguments string `xml:"Actions>Exec>Arguments"`
	}
	dec := xml.NewDecoder(strings.NewReader(doc))
	// The declaration says UTF-16 because that is how the file is written;
	// this string is still the UTF-8 it was rendered as.
	dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	if err := dec.Decode(&task); err != nil {
		t.Fatalf("task XML does not parse: %v\n%s", err, doc)
	}

	if task.TriggerUser != `LAB\alice` || task.Principal.UserID != `LAB\alice` {
		t.Errorf("trigger user %q, principal %q; want the installing user in both (an any-user trigger needs elevation)", task.TriggerUser, task.Principal.UserID)
	}
	if task.Principal.LogonType != "InteractiveToken" || task.Principal.RunLevel != "LeastPrivilege" {
		t.Errorf("principal = %s/%s, want InteractiveToken/LeastPrivilege (the user's desktop session, not elevated)", task.Principal.LogonType, task.Principal.RunLevel)
	}
	if task.Settings.DisallowStartIfOnBatteries != "false" || task.Settings.StopIfGoingOnBatteries != "false" {
		t.Errorf("battery settings = %s/%s, want false/false", task.Settings.DisallowStartIfOnBatteries, task.Settings.StopIfGoingOnBatteries)
	}
	if task.Settings.ExecutionTimeLimit != "PT0S" {
		t.Errorf("ExecutionTimeLimit = %q, want PT0S (no limit)", task.Settings.ExecutionTimeLimit)
	}
	if task.Command != conhost {
		t.Errorf("Command = %q, want %q", task.Command, conhost)
	}
	wantArgs := `--headless "` + self + `" __supervise "--name" "R&D \"lab\" box"`
	if task.Arguments != wantArgs {
		t.Errorf("Arguments = %q\nwant        %q", task.Arguments, wantArgs)
	}
}

func TestUTF16FileRoundTrips(t *testing.T) {
	const s = `<UserId>DESKTOP\用户</UserId>`
	b := utf16File(s)
	if !bytes.HasPrefix(b, []byte{0xFF, 0xFE}) {
		t.Fatalf("missing UTF-16LE byte-order mark: % x", b[:2])
	}
	units := make([]uint16, 0, (len(b)-2)/2)
	for i := 2; i+1 < len(b); i += 2 {
		units = append(units, uint16(b[i])|uint16(b[i+1])<<8)
	}
	if got := string(utf16.Decode(units)); got != s {
		t.Fatalf("round trip = %q, want %q", got, s)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Under the headless console nothing the agent prints is visible, so the
// supervisor has to hand the child's output to the writer it was given
// (agent.log in production) rather than to its own stdout.
func TestSuperviseLoopSendsAgentOutputToTheLog(t *testing.T) {
	if os.Getenv("WANCTL_SUPERVISE_CHILD") == "1" {
		fmt.Println("agent says hello")
		os.Exit(0)
	}
	t.Setenv("WANCTL_SUPERVISE_CHILD", "1")

	out := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- superviseLoop(ctx, os.Args[0], []string{"-test.run=^TestSuperviseLoopSendsAgentOutputToTheLog$"}, out)
	}()

	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "agent says hello") {
		if time.Now().After(deadline) {
			t.Fatalf("the child's output never reached the log writer; got %q", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("superviseLoop returned %v after cancel, want context.Canceled", err)
	}
}
