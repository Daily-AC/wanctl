// Package gui is the device-side local web UI (humans only). It runs on
// localhost and doubles as a policy.Approver: a request that no rule covers pops
// up in the browser as a pending approval; the human clicks y/a/g/n. It also
// shows status, the rule list, and lets the human manage rules and the mode.
package gui

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"wanctl/internal/eventlog"
	"wanctl/internal/policy"
)

//go:embed index.html
var assets embed.FS

// Info is the static status shown in the UI.
type Info struct {
	Device      string `json:"device"`
	Fingerprint string `json:"fingerprint"`
	Relay       string `json:"relay"`
}

type pending struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	Cmd     string          `json:"cmd"`
	Path    string          `json:"path"`
	Cwd     string          `json:"cwd"`
	Peer    string          `json:"peer"`
	Created time.Time       `json:"created"`
	decided chan policy.Decision
	req     policy.Request
}

// Server is the local GUI + approver.
type Server struct {
	engine  *policy.Engine
	log     *eventlog.Logger
	info    Info
	timeout time.Duration

	mu   sync.Mutex
	pend map[string]*pending
	subs map[chan struct{}]struct{}
}

// New builds a GUI server bound to a policy engine and event log.
func New(engine *policy.Engine, log *eventlog.Logger, info Info) *Server {
	return &Server{
		engine:  engine,
		log:     log,
		info:    info,
		timeout: 60 * time.Second,
		pend:    map[string]*pending{},
		subs:    map[chan struct{}]struct{}{},
	}
}

// Ask implements policy.Approver: it surfaces the request in the UI and blocks
// until a human decides or the timeout elapses (then deny).
func (s *Server) Ask(req policy.Request) policy.Decision {
	id := newID()
	p := &pending{
		ID: id, Kind: string(req.Kind), Cmd: req.Cmd, Path: req.Path, Cwd: req.Cwd,
		Peer: req.Peer, Created: time.Now(), decided: make(chan policy.Decision, 1), req: req,
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

// Handler returns the GUI's HTTP mux (serve on localhost only).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		b, _ := assets.ReadFile("index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/decide", s.handleDecide)
	mux.HandleFunc("/api/rules/add", s.handleRuleAdd)
	mux.HandleFunc("/api/rules/rm", s.handleRuleRm)
	mux.HandleFunc("/api/mode", s.handleMode)
	mux.HandleFunc("/api/logs", s.handleLogs)
	return mux
}

func (s *Server) handleLogs(w http.ResponseWriter, _ *http.Request) {
	var events []eventlog.Event
	if s.log != nil {
		events, _ = s.log.Read(eventlog.Filter{Limit: 100})
	}
	writeJSON(w, map[string]any{"events": events})
}

// Serve starts the GUI on addr (e.g. "127.0.0.1:7600") until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.Handler()}
	go func() { <-ctx.Done(); srv.Close() }()
	fmt.Printf("wanctl GUI on http://%s\n", ln.Addr())
	return srv.Serve(ln)
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	pend := make([]*pending, 0, len(s.pend))
	for _, p := range s.pend {
		pend = append(pend, p)
	}
	s.mu.Unlock()
	writeJSON(w, map[string]any{
		"info":    s.info,
		"mode":    s.engine.Mode(),
		"rules":   s.engine.List(),
		"pending": pend,
	})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no stream", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()
	fmt.Fprint(w, "data: hello\n\n")
	flusher.Flush()
	tick := time.NewTicker(25 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			fmt.Fprint(w, "data: change\n\n")
			flusher.Flush()
		case <-tick.C:
			fmt.Fprint(w, "data: ping\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) handleDecide(w http.ResponseWriter, r *http.Request) {
	var body struct{ ID, Verdict string }
	json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	p := s.pend[body.ID]
	s.mu.Unlock()
	if p == nil {
		http.Error(w, "no such request", http.StatusNotFound)
		return
	}
	var d policy.Decision
	switch body.Verdict {
	case "y":
		d = policy.Decision{Allow: true}
	case "a":
		d = policy.Decision{Allow: true, Remember: true, Scope: policy.ScopeDir}
	case "g":
		d = policy.Decision{Allow: true, Remember: true, Scope: policy.ScopeGlobal}
	default:
		d = policy.Decision{Allow: false}
	}
	// The caller (agent gate) records the rule when Remember is set — single
	// source of truth, so we must NOT Add here too (would double-record).
	select {
	case p.decided <- d:
	default:
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleRuleAdd(w http.ResponseWriter, r *http.Request) {
	var rule policy.Rule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		http.Error(w, "bad rule", http.StatusBadRequest)
		return
	}
	if rule.Scope == "" {
		rule.Scope = policy.ScopeGlobal
	}
	s.engine.Add(rule)
	s.notify()
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleRuleRm(w http.ResponseWriter, r *http.Request) {
	var body struct{ Index int }
	json.NewDecoder(r.Body).Decode(&body)
	s.engine.Remove(body.Index)
	s.notify()
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleMode(w http.ResponseWriter, r *http.Request) {
	var body struct{ Mode string }
	json.NewDecoder(r.Body).Decode(&body)
	if body.Mode == string(policy.ModeBypass) {
		s.engine.SetMode(policy.ModeBypass)
	} else {
		s.engine.SetMode(policy.ModeNormal)
	}
	s.notify()
	w.WriteHeader(http.StatusOK)
}

func (s *Server) notify() {
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
