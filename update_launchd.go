package main

import (
	"encoding/xml"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

// managedRestartGOOS is a variable so launchd recovery decisions can be
// exercised on a non-macOS test host without invoking the real service manager.
var managedRestartGOOS = runtime.GOOS

type launchdSnapshot struct {
	domain      string
	label       string
	plistLabel  string
	state       string
	lastExit    string
	properties  string
	plistPath   string
	programPath string
	plistExists bool
	printErr    error
}

func launchdUserDomain() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	if u.Uid == "" {
		return "", fmt.Errorf("current user has no uid")
	}
	return "gui/" + u.Uid, nil
}

// inspectLaunchd reads both launchctl's live record and the plist that owns
// it. The two sources are deliberately checked together: a stale or unrelated
// launchd label must never cause an update to boot out a user's service.
func inspectLaunchd() launchdSnapshot {
	s := launchdSnapshot{label: macLabel}
	s.domain, _ = launchdUserDomain()
	s.plistPath, _ = macPlistPath()
	if s.plistPath != "" {
		if _, err := os.Stat(s.plistPath); err == nil {
			s.plistExists = true
			if raw, err := os.ReadFile(s.plistPath); err == nil {
				p := parseLaunchdPlist(raw)
				s.plistLabel, s.programPath = p.label, p.programPath
				s.label = p.label
			}
		}
	}
	if s.domain == "" {
		s.printErr = fmt.Errorf("resolve launchd user domain")
		return s
	}
	out, err := launchctlRun("print", s.domain+"/"+macLabel)
	s.printErr = err
	if err == nil {
		p := parseLaunchctlPrint(out)
		if p.label != "" {
			s.label = p.label
		}
		s.state, s.lastExit, s.properties = p.state, p.lastExit, p.properties
	}
	return s
}

type launchdPlistFields struct {
	label       string
	programPath string
}

func parseLaunchdPlist(raw []byte) launchdPlistFields {
	var out launchdPlistFields
	dec := xml.NewDecoder(strings.NewReader(string(raw)))
	var key, arrayKey string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "key":
				var v string
				if dec.DecodeElement(&v, &t) == nil {
					key = v
				}
			case "array":
				arrayKey, key = key, ""
			case "string":
				var v string
				if dec.DecodeElement(&v, &t) != nil {
					continue
				}
				if arrayKey == "ProgramArguments" && out.programPath == "" {
					out.programPath = v
				} else if key == "Label" {
					out.label = v
				}
				key = ""
			case "dict":
				// Nothing to do; keys and values are handled above.
			}
		case xml.EndElement:
			if t.Name.Local == "array" {
				arrayKey = ""
			}
		}
	}
	return out
}

type launchctlPrintFields struct {
	label, state, lastExit, properties string
}

func parseLaunchctlPrint(raw string) launchctlPrintFields {
	var out launchctlPrintFields
	headerSeen := false
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case !headerSeen && strings.HasSuffix(line, " = {"):
			name := strings.TrimSuffix(line, " = {")
			if slash := strings.LastIndexByte(name, '/'); slash >= 0 {
				name = name[slash+1:]
			}
			out.label = name
			headerSeen = true
		case strings.HasPrefix(line, "label = "):
			out.label = strings.TrimSpace(strings.TrimPrefix(line, "label = "))
		case strings.HasPrefix(line, "state = "):
			out.state = strings.TrimSpace(strings.TrimPrefix(line, "state = "))
		case strings.HasPrefix(line, "last exit code = "):
			out.lastExit = strings.TrimSpace(strings.TrimPrefix(line, "last exit code = "))
		case strings.HasPrefix(line, "properties = "):
			out.properties = strings.TrimSpace(strings.TrimPrefix(line, "properties = "))
		}
	}
	return out
}

func resolvedPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func launchdRecoveryEligible(s launchdSnapshot, self string) bool {
	if managedRestartGOOS != "darwin" || s.printErr != nil || s.domain == "" || !s.plistExists {
		return false
	}
	if s.label != macLabel || s.plistLabel != macLabel || !strings.Contains(s.properties, "needs LWCR update") || !strings.Contains(s.properties, "managed LWCR") {
		return false
	}
	got, err := resolvedPath(s.programPath)
	if err != nil {
		return false
	}
	want, err := resolvedPath(self)
	return err == nil && got == want
}

func launchdDiagnostic(s launchdSnapshot) string {
	state, exit, props := s.state, s.lastExit, s.properties
	if state == "" {
		state = "<unknown>"
	}
	if exit == "" {
		exit = "<unknown>"
	}
	if props == "" {
		props = "<unknown>"
	}
	return fmt.Sprintf("launchctl print: state=%s; last exit code=%s; properties=%s", state, exit, props)
}

func launchdRecoveryCommand(s launchdSnapshot) string {
	if s.domain == "" {
		return "launchctl bootout gui/<uid>/com.wanctl.agent && launchctl bootstrap gui/<uid> ~/Library/LaunchAgents/com.wanctl.agent.plist"
	}
	return fmt.Sprintf("launchctl bootout %s/%s && launchctl bootstrap %s %s", s.domain, macLabel, s.domain, s.plistPath)
}

// recoverLaunchd performs exactly one bootout/bootstrap pair. A failed
// bootout is terminal: bootstrap must never be attempted against an unknown
// service state.
func recoverLaunchd(s launchdSnapshot) error {
	if _, err := launchctlRun("bootout", s.domain+"/"+macLabel); err != nil {
		return fmt.Errorf("launchctl bootout: %w", err)
	}
	if _, err := launchctlRun("bootstrap", s.domain, s.plistPath); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w", err)
	}
	return nil
}

func recoverLaunchdAndAwait(s launchdSnapshot, wait func() managedRestartResult) (managedRestartResult, error) {
	if err := recoverLaunchd(s); err != nil {
		return managedRestartStuck, err
	}
	return wait(), nil
}
