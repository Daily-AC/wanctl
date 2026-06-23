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
)

// Request is an operation awaiting an authorization decision.
type Request struct {
	Kind Kind
	Cmd  string // exec: the command line
	Path string // file ops: target path
	Cwd  string // exec: working directory (may be "")
	Peer string // peer fingerprint/name (for logging)
}

// Rule is one persisted allow-list entry.
type Rule struct {
	Kind    Kind      `json:"kind"`
	Pattern string    `json:"pattern"` // exec: command (+arg prefix, trailing * ok); file: directory ("" = any)
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
	mu    sync.Mutex
	rules []Rule
	mode  Mode
	path  string
}

// Open loads (or initializes) the named rule file in the config dir.
func Open(name string, mode Mode) (*Engine, error) {
	dir, err := transport.ConfigDir()
	if err != nil {
		return nil, err
	}
	if mode == "" {
		mode = ModeNormal
	}
	e := &Engine{path: filepath.Join(dir, name), mode: mode}
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

// Mode reports the current mode.
func (e *Engine) Mode() Mode {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mode
}

// SetMode changes the mode.
func (e *Engine) SetMode(m Mode) {
	e.mu.Lock()
	e.mode = m
	e.mu.Unlock()
}

// Allowed reports whether a request is already permitted by some rule.
func (e *Engine) Allowed(req Request) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range e.rules {
		if ruleMatches(r, req) {
			return true
		}
	}
	return false
}

func ruleMatches(r Rule, req Request) bool {
	switch req.Kind {
	case KindExec:
		if r.Kind != KindExec || !MatchCommand(r.Pattern, req.Cmd) {
			return false
		}
		return r.Scope == ScopeGlobal || (r.Scope == ScopeDir && r.Dir == req.Cwd && req.Cwd != "")
	case KindRead:
		// A read is satisfied by a read OR write rule covering the path.
		return (r.Kind == KindRead || r.Kind == KindWrite) && Within(r.Pattern, req.Path)
	case KindWrite:
		return r.Kind == KindWrite && Within(r.Pattern, req.Path)
	}
	return false
}

// MatchCommand reports whether command c matches pattern p. A trailing " *"
// matches any suffix; otherwise c must equal p or start with "p ".
func MatchCommand(p, c string) bool {
	p = strings.TrimSpace(p)
	c = strings.TrimSpace(c)
	if strings.HasSuffix(p, " *") {
		return strings.HasPrefix(c, p[:len(p)-1]) // keep the space before *
	}
	if p == "*" {
		return true
	}
	return c == p || strings.HasPrefix(c, p+" ")
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
	case KindExec:
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
