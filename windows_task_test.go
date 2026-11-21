package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
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
	for _, major := range []uint32{6, 10} {
		command, args := winTaskAction(conhost, testPowerShell, self, []string{"--name", `R&D "lab" box`}, major)
		checkWinTaskXML(t, winTaskXML(`LAB\alice`, command, args), command, args)
	}
	command, args := winTaskAction(conhost, testPowerShell, self, []string{"--name", `R&D "lab" box`}, 10)
	if command != conhost {
		t.Errorf("Command = %q, want %q", command, conhost)
	}
	wantArgs := `--headless "` + self + `" __supervise "--name" "R&D \"lab\" box"`
	if args != wantArgs {
		t.Errorf("Arguments = %q\nwant        %q", args, wantArgs)
	}
}

func checkWinTaskXML(t *testing.T, doc, command, args string) {
	t.Helper()

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
	if task.Command != command || task.Arguments != args {
		t.Errorf("action = %q %q, want %q %q", task.Command, task.Arguments, command, args)
	}
}

const testPowerShell = `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`

// The Windows 10 task is the one every existing install runs; the Windows 7
// launcher must not change a byte of it. The golden text is what winTaskXML
// rendered for these inputs before the launcher became a choice.
func TestWinTaskXMLWindows10IsUnchanged(t *testing.T) {
	const golden = `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>wanctl agent (remote device control)</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>LAB\alice</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>LAB\alice</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>C:\Windows\System32\conhost.exe</Command>
      <Arguments>--headless &quot;C:\Users\O'Brien\My Apps\wanctl.exe&quot; __supervise &quot;--name&quot; &quot;R&amp;D \&quot;lab\&quot; box&quot; &quot;--relay&quot; &quot;https://relay.example.com&quot;</Arguments>
    </Exec>
  </Actions>
</Task>
`
	command, args := winTaskAction(`C:\Windows\System32\conhost.exe`, testPowerShell,
		`C:\Users\O'Brien\My Apps\wanctl.exe`,
		[]string{"--name", `R&D "lab" box`, "--relay", "https://relay.example.com"}, 10)
	if got := winTaskXML(`LAB\alice`, command, args); got != golden {
		t.Fatalf("Windows 10 task XML changed:\n%s\nwant\n%s", got, golden)
	}
}

// conhost has no --headless before Windows 10, so there the task runs the
// supervisor under Windows PowerShell 2.0's hidden window. Its argument string
// passes through three parsers before wanctl sees it: powershell.exe splits
// its command line, PowerShell parses the -Command script, and PowerShell then
// rebuilds a command line for wanctl, which Go splits again. Each is modelled
// here on its own, and both of PowerShell's known rules for that rebuild are
// tried, so the test fails if any layer would hand wanctl other arguments.
func TestWinTaskActionBelowWindows10(t *testing.T) {
	for _, tc := range []struct {
		name  string
		self  string
		extra []string
	}{
		{"path with spaces", `C:\Users\Ann Lee\App Data\wanctl\wanctl.exe`, []string{"--relay", "https://relay.example.com"}},
		{"path with a quote", `C:\Users\O'Brien\AppData\Local\wanctl\wanctl.exe`, []string{"--name", "o'brien's pc"}},
		{"args with spaces and quotes", `C:\wanctl\wanctl.exe`, []string{"--name", `R&D "lab" box`, "--portal-fps", `a"b`, "--relay", "https://relay.example.com"}},
		{"backslashes before quotes and at the end", `C:\wanctl\wanctl.exe`, []string{"--name", `C:\dir with space\`, "--name", `x\"y z`, "--name", `C:\nospace\`}},
		{"typographic quote and PowerShell syntax", `C:\wanctl\wanctl.exe`, []string{"--name", "Ann’s $(rm) `box` ; &"}},
		{"empty argument", `C:\wanctl\wanctl.exe`, []string{"--name", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command, args := winTaskAction(`C:\Windows\System32\conhost.exe`, testPowerShell, tc.self, tc.extra, 6)
			if command != testPowerShell {
				t.Fatalf("Command = %q, want Windows PowerShell", command)
			}
			argv := commandLineToArgvForTest(args)
			if len(argv) != 5 || strings.Join(argv[:4], " ") != "-NoProfile -WindowStyle Hidden -Command" {
				t.Fatalf("powershell.exe would see %q", argv)
			}
			words, err := parsePSCallForTest(argv[4])
			if err != nil {
				t.Fatalf("script %q: %v", argv[4], err)
			}
			if words[0] != tc.self || words[1] != "__supervise" {
				t.Fatalf("script %q calls %q", argv[4], words)
			}
			want := append([]string{tc.self, "__supervise"}, tc.extra...)
			for rule, needQuotes := range map[string]func(string) bool{
				"quote on whitespace":                func(a string) bool { return strings.IndexFunc(a, unicode.IsSpace) >= 0 },
				"quote on whitespace outside quotes": psNeedQuotesForTest,
			} {
				// PowerShell puts the program path first, quoted, then each
				// argument as it is, in double quotes when the rule says so, and
				// leaves out empty ones.
				line := `"` + words[0] + `"`
				for _, w := range words[1:] {
					switch {
					case w == "":
					case needQuotes(w):
						line += ` "` + w + `"`
					default:
						line += " " + w
					}
				}
				if got := commandLineToArgvForTest(line); !slices.Equal(got, want) {
					t.Errorf("%s: wanctl would see %q\nwant %q\ncommand line %s", rule, got, want, line)
				}
			}
		})
	}
}

// commandLineToArgvForTest is Go's own Windows command-line splitter
// (os.commandLineToArgv), the one that builds os.Args in wanctl.exe; for the
// arguments built here powershell.exe's C runtime splits the same way.
func commandLineToArgvForTest(cmd string) []string {
	var args []string
	for len(cmd) > 0 {
		if cmd[0] == ' ' || cmd[0] == '\t' {
			cmd = cmd[1:]
			continue
		}
		var b []byte
		inquote, nslash := false, 0
		for ; len(cmd) > 0; cmd = cmd[1:] {
			c := cmd[0]
			if (c == ' ' || c == '\t') && !inquote {
				cmd = cmd[1:]
				break
			}
			switch c {
			case '"':
				b = append(b, bytes.Repeat([]byte{'\\'}, nslash/2)...)
				if nslash%2 == 0 {
					if inquote && len(cmd) > 1 && cmd[1] == '"' {
						b = append(b, c)
						cmd = cmd[1:]
					}
					inquote = !inquote
				} else {
					b = append(b, c)
				}
				nslash = 0
				continue
			case '\\':
				nslash++
				continue
			}
			b = append(b, bytes.Repeat([]byte{'\\'}, nslash)...)
			nslash = 0
			b = append(b, c)
		}
		args = append(args, string(b)+strings.Repeat(`\`, nslash))
	}
	return args
}

// parsePSCallForTest reads a script of the form `& 'program' word 'arg' ...`
// the way PowerShell's tokenizer does: single-quoted strings, including the
// typographic quotes, with a doubled quote standing for one.
func parsePSCallForTest(script string) ([]string, error) {
	isQuote := func(r rune) bool { return strings.ContainsRune("'‘’‚‛", r) }
	rs := []rune(script)
	if len(rs) < 2 || rs[0] != '&' || rs[1] != ' ' {
		return nil, fmt.Errorf("not a call: %q", script)
	}
	var words []string
	for i := 2; i < len(rs); {
		if rs[i] == ' ' {
			i++
			continue
		}
		if !isQuote(rs[i]) {
			j := i
			for j < len(rs) && rs[j] != ' ' {
				j++
			}
			if word := string(rs[i:j]); word != "__supervise" {
				return nil, fmt.Errorf("unquoted word %q", word)
			}
			words = append(words, string(rs[i:j]))
			i = j
			continue
		}
		var w []rune
		for i++; ; i++ {
			if i >= len(rs) {
				return nil, fmt.Errorf("unterminated string in %q", script)
			}
			if isQuote(rs[i]) {
				if i+1 < len(rs) && isQuote(rs[i+1]) {
					w = append(w, rs[i])
					i++
					continue
				}
				i++
				break
			}
			w = append(w, rs[i])
		}
		if i < len(rs) && rs[i] != ' ' {
			return nil, fmt.Errorf("string runs into %q", string(rs[i:]))
		}
		words = append(words, string(w))
	}
	return words, nil
}

// psNeedQuotesForTest is Windows PowerShell 5.1's NeedQuotes: whitespace
// outside a double-quoted section that is not opened by an escaped quote.
func psNeedQuotesForTest(s string) bool {
	followingBackslash, inQuote := false, false
	for _, r := range s {
		if r == '"' && !followingBackslash {
			inQuote = !inQuote
		} else if unicode.IsSpace(r) && !inQuote {
			return true
		}
		followingBackslash = r == '\\'
	}
	return false
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
