// Package relay is the public WebSocket broker. It authenticates endpoints by
// token, keeps an in-memory registry of online devices, and byte-pipes a
// controller's session to the matching agent. It never terminates or inspects
// the end-to-end TLS that flows through it.
package relay

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"wanctl/internal/wsconn"

	"github.com/coder/websocket"
)

type agentConn struct {
	device string
	ns     string
	ctrl   io.Writer
	mu     sync.Mutex // serialize control writes
}

func (a *agentConn) send(v any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return json.NewEncoder(a.ctrl).Encode(v)
}

type pendingSession struct {
	agentSide  chan io.ReadWriteCloser
	clientSide chan io.ReadWriteCloser
}

// Relay is the broker.
type Relay struct {
	ts TokenStore

	mu      sync.Mutex
	agents  map[string]*agentConn      // key "ns/device" (WebSocket transport)
	pending map[string]*pendingSession // key session id (WebSocket transport)

	hmu     sync.Mutex
	hagents map[string]*httpAgent   // key "ns/device" (HTTP transport)
	hsess   map[string]*httpSession // key session id (HTTP transport)
}

// New constructs a Relay backed by the given TokenStore.
func New(ts TokenStore) *Relay {
	return &Relay{
		ts:      ts,
		agents:  map[string]*agentConn{},
		pending: map[string]*pendingSession{},
		hagents: map[string]*httpAgent{},
		hsess:   map[string]*httpSession{},
	}
}

// Handler returns the relay's HTTP mux.
func (r *Relay) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/agent", r.handleAgent)
	mux.HandleFunc("/dial", r.handleDial)
	mux.HandleFunc("/session/", r.handleSession)
	mux.HandleFunc("/peers", r.handlePeers)
	// HTTP transport (proxy-agnostic; no WebSocket upgrade required).
	mux.HandleFunc("/h/poll", r.handleHPoll)
	mux.HandleFunc("/h/dial", r.handleHDial)
	mux.HandleFunc("/h/peers", r.handleHPeers)
	mux.HandleFunc("/h/up", r.handleHUp)
	mux.HandleFunc("/h/down", r.handleHDown)
	mux.HandleFunc("/h/close", r.handleHClose)
	return mux
}

func (r *Relay) auth(req *http.Request) (ns string, ok bool) {
	return r.ts.Resolve(req.URL.Query().Get("token"))
}

func newID() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (r *Relay) handleAgent(w http.ResponseWriter, req *http.Request) {
	ns, ok := r.auth(req)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	c, err := websocket.Accept(w, req, nil)
	if err != nil {
		return
	}
	nc := wsconn.FromAccepted(req.Context(), c)
	dec := json.NewDecoder(nc)
	var reg struct {
		Op, Device string
	}
	if err := dec.Decode(&reg); err != nil || reg.Op != "register" || reg.Device == "" {
		c.Close(websocket.StatusPolicyViolation, "expected register")
		return
	}
	key := ns + "/" + reg.Device
	ac := &agentConn{device: reg.Device, ns: ns, ctrl: nc}
	r.mu.Lock()
	r.agents[key] = ac
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		if r.agents[key] == ac {
			delete(r.agents, key)
		}
		r.mu.Unlock()
		c.Close(websocket.StatusNormalClosure, "")
	}()
	// Keep the control connection alive; drain any further messages (e.g. pings).
	for {
		var ignore json.RawMessage
		if err := dec.Decode(&ignore); err != nil {
			return
		}
	}
}

func (r *Relay) handleDial(w http.ResponseWriter, req *http.Request) {
	ns, ok := r.auth(req)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	target := req.URL.Query().Get("target") // "NS/DEVICE"; default NS = caller's
	if !strings.Contains(target, "/") {
		target = ns + "/" + target
	}
	// Foundation milestone: only same-namespace access (ACL is a later milestone).
	if !strings.HasPrefix(target, ns+"/") {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	r.mu.Lock()
	ac := r.agents[target]
	r.mu.Unlock()
	if ac == nil {
		http.Error(w, "device offline", http.StatusNotFound)
		return
	}
	sid := newID()
	ps := &pendingSession{agentSide: make(chan io.ReadWriteCloser, 1), clientSide: make(chan io.ReadWriteCloser, 1)}
	r.mu.Lock()
	r.pending[sid] = ps
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.pending, sid); r.mu.Unlock() }()

	if err := ac.send(map[string]string{"op": "open", "session": sid, "url": "/session/" + sid + "?token=" + req.URL.Query().Get("token")}); err != nil {
		http.Error(w, "agent unreachable", http.StatusBadGateway)
		return
	}
	c, err := websocket.Accept(w, req, nil)
	if err != nil {
		return
	}
	clientNC := wsconn.FromAccepted(req.Context(), c)
	ps.clientSide <- clientNC

	select {
	case agentNC := <-ps.agentSide:
		pipe(clientNC, agentNC)
	case <-time.After(15 * time.Second):
		c.Close(websocket.StatusBadGateway, "agent did not open session")
	}
}

func (r *Relay) handleSession(w http.ResponseWriter, req *http.Request) {
	if _, ok := r.auth(req); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sid := strings.TrimPrefix(req.URL.Path, "/session/")
	r.mu.Lock()
	ps := r.pending[sid]
	r.mu.Unlock()
	if ps == nil {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	c, err := websocket.Accept(w, req, nil)
	if err != nil {
		return
	}
	agentNC := wsconn.FromAccepted(req.Context(), c)
	ps.agentSide <- agentNC
	select {
	case clientNC := <-ps.clientSide:
		pipe(agentNC, clientNC)
	case <-time.After(15 * time.Second):
		c.Close(websocket.StatusBadGateway, "client went away")
	}
}

func (r *Relay) handlePeers(w http.ResponseWriter, req *http.Request) {
	ns, ok := r.auth(req)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	devices := []string{}
	r.mu.Lock()
	for key, ac := range r.agents {
		if ac.ns == ns {
			devices = append(devices, strings.TrimPrefix(key, ns+"/"))
		}
	}
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"namespace": ns, "devices": devices})
}

// pipe copies bytes both directions until either side closes, then tears down.
func pipe(a, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }()
	go func() { io.Copy(b, a); done <- struct{}{} }()
	<-done
	a.Close()
	b.Close()
}
