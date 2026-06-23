// Package console is the transport-neutral device console: a policy.Approver
// backed by a pending-approval queue plus rule/mode/log accessors. The agent
// uses it both as its approver and as the backend for remote (portal) console
// sessions. The CLI terminal and a remote portal feed decisions into the same
// queue — first answer wins.
package console

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"wanctl/internal/eventlog"
	"wanctl/internal/policy"
)

// Info is static device identity shown in the console.
type Info struct {
	Device      string `json:"device"`
	Fingerprint string `json:"fingerprint"`
	Relay       string `json:"relay"`
}

// Pending is one approval awaiting a decision (JSON-safe view).
type Pending struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"`
	Cmd     string    `json:"cmd"`
	Path    string    `json:"path"`
	Cwd     string    `json:"cwd"`
	Peer    string    `json:"peer"`
	Created time.Time `json:"created"`
}

// State is a full snapshot for a console front-end.
type State struct {
	Info    Info          `json:"info"`
	Mode    policy.Mode   `json:"mode"`
	Rules   []policy.Rule `json:"rules"`
	Pending []Pending     `json:"pending"`
}

type pending struct {
	view    Pending
	decided chan policy.Decision
}

// Service is the queue-backed console + approver.
type Service struct {
	engine  *policy.Engine
	log     *eventlog.Logger
	info    Info
	timeout time.Duration

	mu   sync.Mutex
	pend map[string]*pending
	subs map[chan struct{}]struct{}
}

// New builds a console service bound to a policy engine and (optional) event log.
func New(engine *policy.Engine, log *eventlog.Logger, info Info) *Service {
	return &Service{
		engine: engine, log: log, info: info, timeout: 60 * time.Second,
		pend: map[string]*pending{}, subs: map[chan struct{}]struct{}{},
	}
}

// Ask implements policy.Approver: enqueue and block until a front-end decides
// or the timeout elapses (then deny).
func (s *Service) Ask(req policy.Request) policy.Decision {
	id := newID()
	p := &pending{
		view: Pending{
			ID: id, Kind: string(req.Kind), Cmd: req.Cmd, Path: req.Path,
			Cwd: req.Cwd, Peer: req.Peer, Created: time.Now(),
		},
		decided: make(chan policy.Decision, 1),
	}
	s.mu.Lock()
	s.pend[id] = p
	s.mu.Unlock()
	s.notify()
	defer func() {
		s.mu.Lock()
		delete(s.pend, id)
		s.mu.Unlock()
		s.notify()
	}()
	select {
	case d := <-p.decided:
		return d
	case <-time.After(s.timeout):
		return policy.Decision{Allow: false}
	}
}

// State returns a snapshot for a front-end.
func (s *Service) State() State {
	s.mu.Lock()
	pend := make([]Pending, 0, len(s.pend))
	for _, p := range s.pend {
		pend = append(pend, p.view)
	}
	s.mu.Unlock()
	return State{Info: s.info, Mode: s.engine.Mode(), Rules: s.engine.List(), Pending: pend}
}

// Decide delivers a verdict to a pending request. Returns false if unknown.
// Verdict maps y/a/g/n exactly like the console approver. The agent gate is the
// single source of truth for remembering rules, so we never Add here.
func (s *Service) Decide(id, verdict string) bool {
	s.mu.Lock()
	p := s.pend[id]
	s.mu.Unlock()
	if p == nil {
		return false
	}
	var d policy.Decision
	switch verdict {
	case "y", "yes":
		d = policy.Decision{Allow: true}
	case "a":
		d = policy.Decision{Allow: true, Remember: true, Scope: policy.ScopeDir}
	case "g":
		d = policy.Decision{Allow: true, Remember: true, Scope: policy.ScopeGlobal}
	default:
		d = policy.Decision{Allow: false}
	}
	select {
	case p.decided <- d:
	default:
	}
	return true
}

// AddRule / RemoveRule / SetMode proxy the engine and notify subscribers.
func (s *Service) AddRule(r policy.Rule) error { err := s.engine.Add(r); s.notify(); return err }
func (s *Service) RemoveRule(i int) error      { err := s.engine.Remove(i); s.notify(); return err }
func (s *Service) SetMode(m policy.Mode)       { s.engine.SetMode(m); s.notify() }

// Logs returns the last `limit` events (0 -> 100).
func (s *Service) Logs(limit int) []eventlog.Event {
	if s.log == nil {
		return nil
	}
	if limit <= 0 {
		limit = 100
	}
	ev, _ := s.log.Read(eventlog.Filter{Limit: limit})
	return ev
}

// Subscribe returns a channel pinged on any state change, plus a cancel func.
func (s *Service) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}
}

func (s *Service) notify() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func newID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
