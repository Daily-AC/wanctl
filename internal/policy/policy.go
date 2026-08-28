// Package policy implements the device-side permission engine: a Claude-Code-style
// allow-list of command rules and file roots, with per-directory or global scope
// and a normal/bypass mode. Rules are JSON-persisted in the agent config dir and
// only ever live on the controlled device.
package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mvdan.cc/sh/v3/syntax"

	"wanctl/internal/transport"
)

// Mode controls what happens on a rule miss.
type Mode string

const (
	ModeNormal Mode = "normal" // prompt for approval on a miss
	ModeBypass Mode = "bypass" // auto-allow everything (dangerous)
)

// Scope is how broadly a remembered rule applies.
type Scope string

const (
	ScopeDir    Scope = "dir"    // only within a specific directory
	ScopeGlobal Scope = "global" // anywhere
)

// Kind is the operation being authorized.
type Kind string

const (
	KindExec  Kind = "exec"
	KindRead  Kind = "read"
	KindWrite Kind = "write"
	KindLogs  Kind = "logs"

	// KindExecElevated is a command run through an elevation channel (root,
	// or the device's own adbd — see internal/elevate). It is a class
	// of its own, not a flag on KindExec, for two reasons:
	//
	//   - An `exec` rule must not authorize the elevated form of the same
	//     command. "You may run `pm list packages`" and "you may run it as uid
	//     2000" are different grants and a device owner approved only one.
	//   - Bypass mode does not cover it (see Engine.Bypasses). The 自动放行
	//     switch exists so an unattended device is usable at all; handing out
	//     root as a side effect of that would be a decision nobody made.
	KindExecElevated Kind = "exec-elevated"
)

// Request is an operation awaiting an authorization decision.
type Request struct {
	Kind Kind
	Cmd  string // exec: the command line
	Path string // file ops: target path
	Cwd  string // exec: working directory (may be "")
	Peer string // peer fingerprint/name (for logging)
	// Via is the elevation channel the controller asked for, or "" for
	// whichever one the device picks. It is shown to whoever approves the
	// command and is never part of rule matching: a rule authorizes a command,
	// not a route to running it.
	Via string
}

// Rule is one persisted allow-list entry.
type Rule struct {
	Kind    Kind      `json:"kind"`
	Pattern string    `json:"pattern"` // exec: command (single-command arg prefix, trailing * ok); file: directory ("" = any)
	Dir     string    `json:"dir,omitempty"`
	Scope   Scope     `json:"scope"`
	Added   time.Time `json:"added"`
}

// Decision is an approver's verdict.
type Decision struct {
	Allow    bool
	Remember bool
	Scope    Scope
}

// Engine holds the rule set and mode for a device.
type Engine struct {
	mu       sync.Mutex
	rules    []Rule
	mode     Mode
	path     string
	modePath string
	// psShell is true when this device's session shell is PowerShell, whose
	// argument binding eagerly evaluates @(...), .{...}, &{...} and backtick
	// sequences the POSIX command parser cannot see. A prefix rule must not
	// authorize a command carrying one (audit 2026-08-28, SEC-D1-02).
	psShell bool
}

// SetPowerShell records whether the device runs a PowerShell session shell, so
// prefix rules are matched conservatively against PowerShell's own evaluation
// syntax. The agent calls it once at startup from its resolved shell.
func (e *Engine) SetPowerShell(v bool) {
	e.mu.Lock()
	e.psShell = v
	e.mu.Unlock()
}

// Open loads (or initializes) the named rule file in the config dir. The mode
// argument is the requested mode: a non-empty value (an explicit --mode flag)
// wins and is remembered; an empty value falls back to the persisted mode from a
// previous run, then to ModeNormal. This is why a `bypass` set at runtime (or via
// an explicit flag) survives an agent restart instead of silently reverting.
func Open(name string, mode Mode) (*Engine, error) {
	dir, err := transport.ConfigDir()
	if err != nil {
		return nil, err
	}
	e := &Engine{path: filepath.Join(dir, name), modePath: filepath.Join(dir, "mode")}

	explicit := mode != ""
	if !explicit {
		mode = e.loadMode() // "" if no/invalid persisted file
	}
	if mode == "" {
		mode = ModeNormal
	}
	e.mode = mode
	if explicit {
		// An explicit --mode is a deliberate choice; record it so the next
		// flag-less restart (e.g. a keeper/service launch) keeps it.
		_ = e.saveMode()
	}

	data, err := os.ReadFile(e.path)
	if err != nil {
		if os.IsNotExist(err) {
			return e, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, &e.rules); err != nil {
		return nil, err
	}
	return e, nil
}

// loadMode reads the persisted mode, returning "" if absent or invalid.
func (e *Engine) loadMode() Mode {
	b, err := os.ReadFile(e.modePath)
	if err != nil {
		return ""
	}
	switch m := Mode(strings.TrimSpace(string(b))); m {
	case ModeNormal, ModeBypass:
		return m
	default:
		return ""
	}
}

// saveMode persists the current mode so it survives a restart.
func (e *Engine) saveMode() error {
	if e.modePath == "" {
		return nil
	}
	return os.WriteFile(e.modePath, []byte(string(e.Mode())+"\n"), 0o600)
}

// Mode reports the current mode.
func (e *Engine) Mode() Mode {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mode
}

// SetMode changes the mode and persists it, so a runtime switch to bypass (e.g.
// from the portal toggle) outlives an agent restart.
func (e *Engine) SetMode(m Mode) {
	e.mu.Lock()
	e.mode = m
	e.mu.Unlock()
	_ = e.saveMode()
}

// Allowed reports whether a request is already permitted by some rule.
func (e *Engine) Allowed(req Request) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range e.rules {
		if ruleMatchesShell(r, req, e.psShell) {
			return true
		}
	}
	return false
}

// AllowedFileRoot reports the directory rule that authorizes a file request.
// The caller must bind the actual filesystem open to this root; a boolean
// policy decision followed by an unrestricted open would permit symlink escape.
func (e *Engine) AllowedFileRoot(req Request) (string, bool) {
	if req.Kind != KindRead && req.Kind != KindWrite {
		return "", false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range e.rules {
		if ruleMatchesShell(r, req, e.psShell) {
			return r.Pattern, true
		}
	}
	return "", false
}

// Bypasses reports whether bypass mode alone authorizes this kind of request.
// Everything does except elevation: a device left in bypass so it can work
// unattended has not thereby agreed to run commands as root.
func (e *Engine) Bypasses(kind Kind) bool {
	return e.Mode() == ModeBypass && kind != KindExecElevated
}

func ruleMatches(r Rule, req Request) bool { return ruleMatchesShell(r, req, false) }

func ruleMatchesShell(r Rule, req Request, psShell bool) bool {
	switch req.Kind {
	case KindExec, KindExecElevated:
		// The rule's kind must equal the request's: an exec rule never covers
		// an elevated command, and an elevated rule never covers a plain one
		// (it would be a wider grant than the command needs).
		if r.Kind != req.Kind || !matchCommand(r.Pattern, req.Cmd, psShell) {
			return false
		}
		return r.Scope == ScopeGlobal || (r.Scope == ScopeDir && r.Dir == req.Cwd && req.Cwd != "")
	case KindRead:
		// A read is satisfied by a read OR write rule covering the path.
		return (r.Kind == KindRead || r.Kind == KindWrite) && Within(r.Pattern, req.Path)
	case KindWrite:
		return r.Kind == KindWrite && Within(r.Pattern, req.Path)
	case KindLogs:
		return r.Kind == KindLogs && r.Scope == ScopeGlobal
	}
	return false
}

// MatchCommand reports whether command c matches pattern p. Exact rules may
// contain shell operators. Prefix rules only extend a single simple command,
// so an argv prefix cannot authorize another command, a command substitution,
// or a redirection. A trailing " *" explicitly matches any argument suffix.
func MatchCommand(p, c string) bool { return matchCommand(p, c, false) }

func matchCommand(p, c string, psShell bool) bool {
	p = strings.TrimSpace(p)
	c = strings.TrimSpace(c)
	if c == p {
		return true
	}
	if p == "*" {
		return true
	}
	if !isSingleSimpleCommand(c) {
		return false
	}
	// On a PowerShell device the POSIX parser above is blind to PowerShell's
	// own evaluation syntax, so `ping @(Remove-Item x)` reads as one simple
	// command and a `ping *` prefix rule would authorize it. A prefix rule
	// never covers a command that carries such a sequence; an exact rule
	// (handled by the c == p branch) still can (audit 2026-08-28, SEC-D1-02).
	if psShell && hasPowerShellEval(c) {
		return false
	}
	if strings.HasSuffix(p, " *") {
		return strings.HasPrefix(c, p[:len(p)-1]) // keep the space before *
	}
	return strings.HasPrefix(c, p+" ")
}

// hasPowerShellEval reports whether c contains a PowerShell construct that
// evaluates code during argument binding: a subexpression @(...) or $(...), a
// call/dot-source scriptblock &{...} / .{...}, a backtick escape, or a
// statement separator inside what the POSIX parser took for one argument.
func hasPowerShellEval(c string) bool {
	for _, seq := range []string{"@(", "$(", "&{", ".{", "${", "`", ";", "|", "&"} {
		if strings.Contains(c, seq) {
			return true
		}
	}
	return false
}

func isSingleSimpleCommand(command string) bool {
	f, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil || len(f.Stmts) != 1 {
		return false
	}
	stmt := f.Stmts[0]
	if stmt.Negated || stmt.Background || stmt.Coprocess || stmt.Disown ||
		stmt.Semicolon.IsValid() || len(stmt.Redirs) != 0 {
		return false
	}
	if _, ok := stmt.Cmd.(*syntax.CallExpr); !ok {
		return false
	}
	safe := true
	syntax.Walk(stmt, func(node syntax.Node) bool {
		switch node.(type) {
		case *syntax.CmdSubst, *syntax.ProcSubst:
			safe = false
			return false
		}
		return safe
	})
	return safe
}

// Within reports whether path is dir or under dir. An empty dir matches any path.
func Within(dir, path string) bool {
	if dir == "" {
		return true
	}
	dir = filepath.Clean(dir)
	path = filepath.Clean(path)
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// RuleFor builds the rule to remember for a request at the given scope.
func RuleFor(req Request, scope Scope) Rule {
	r := Rule{Kind: req.Kind, Scope: scope, Added: time.Now()}
	switch req.Kind {
	case KindExec, KindExecElevated:
		r.Pattern = strings.TrimSpace(req.Cmd)
		if scope == ScopeDir && req.Cwd != "" {
			r.Dir = req.Cwd
		} else {
			r.Scope = ScopeGlobal // dir scope is meaningless without a cwd
		}
	case KindRead, KindWrite:
		if scope == ScopeDir {
			r.Pattern = filepath.Dir(req.Path)
		} else {
			r.Pattern = "" // any path
		}
	case KindLogs:
		r.Pattern = "*"
		r.Scope = ScopeGlobal
	}
	return r
}

// Add appends a rule and persists.
func (e *Engine) Add(r Rule) error {
	e.mu.Lock()
	if r.Added.IsZero() {
		r.Added = time.Now()
	}
	e.rules = append(e.rules, r)
	e.mu.Unlock()
	return e.save()
}

// List returns a copy of the rules.
func (e *Engine) List() []Rule {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Rule, len(e.rules))
	copy(out, e.rules)
	return out
}

// Remove deletes the rule at a 0-based index and persists.
func (e *Engine) Remove(i int) error {
	e.mu.Lock()
	if i < 0 || i >= len(e.rules) {
		e.mu.Unlock()
		return os.ErrInvalid
	}
	e.rules = append(e.rules[:i], e.rules[i+1:]...)
	e.mu.Unlock()
	return e.save()
}

func (e *Engine) save() error {
	e.mu.Lock()
	data, err := json.MarshalIndent(e.rules, "", "  ")
	path := e.path
	e.mu.Unlock()
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
