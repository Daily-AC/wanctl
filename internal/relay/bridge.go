package relay

import (
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"wanctl/internal/limits"
	"wanctl/internal/sessionauth"
	"wanctl/internal/wsconn"

	"github.com/coder/websocket"
)

const agentSessionOpenTimeout = 15 * time.Second

// httpSessionConn adapts one role of an HTTP session to the byte stream used by
// the WebSocket transport. It only moves opaque chunks between the queues.
type httpSessionConn struct {
	readQ  *sideQueue
	writeQ *sideQueue
	close  func()

	readMu  sync.Mutex
	readBuf []byte
	once    sync.Once
}

func (c *httpSessionConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for len(c.readBuf) == 0 {
		data, closed := c.readQ.drain(downPollWait)
		if len(data) > 0 {
			c.readBuf = data
			break
		}
		if closed {
			return 0, io.EOF
		}
	}
	n := copy(p, c.readBuf)
	c.readBuf = c.readBuf[n:]
	return n, nil
}

func (c *httpSessionConn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if !c.writeQ.push(p) {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}

func (c *httpSessionConn) Close() error {
	c.once.Do(c.close)
	return nil
}

func (r *Relay) newHTTPSession(sid string) *httpSession {
	s := &httpSession{toClient: newSideQueue(), toAgent: newSideQueue()}
	r.hmu.Lock()
	r.hsess[sid] = s
	r.hmu.Unlock()
	return s
}

func (r *Relay) closeHTTPSession(sid string, s *httpSession) {
	r.hmu.Lock()
	if r.hsess[sid] == s {
		delete(r.hsess, sid)
	}
	r.hmu.Unlock()
	s.close()
}

func (r *Relay) httpSessionConn(sid string, s *httpSession, role string) io.ReadWriteCloser {
	readQ, writeQ := s.toClient, s.toAgent
	if role == "agent" {
		readQ, writeQ = s.toAgent, s.toClient
	}
	return &httpSessionConn{
		readQ:  readQ,
		writeQ: writeQ,
		close:  func() { r.closeHTTPSession(sid, s) },
	}
}

func (r *Relay) handleWSDialToHTTP(w http.ResponseWriter, req *http.Request, targetKey string, auth sessionauth.Open) {
	r.hmu.Lock()
	a := r.hagents[targetKey]
	if a == nil || time.Since(a.lastSeen) > httpAgentTTL {
		r.hmu.Unlock()
		http.Error(w, "device offline", http.StatusNotFound)
		return
	}
	sid := newID()
	auth.Session = sid
	s := &httpSession{toClient: newSideQueue(), toAgent: newSideQueue()}
	r.hsess[sid] = s
	r.hmu.Unlock()

	select {
	case a.open <- auth:
	default:
		r.closeHTTPSession(sid, s)
		http.Error(w, "agent busy", http.StatusServiceUnavailable)
		return
	}
	if r.audit != nil {
		r.audit.Audit(auth.OwnerNamespace, auth.Device, "dial")
	}
	c, err := websocket.Accept(w, req, nil)
	if err != nil {
		r.closeHTTPSession(sid, s)
		return
	}
	limits.ClearHijackedDeadline(req.Context())
	clientNC := wsconn.FromAccepted(req.Context(), c)
	pipe(clientNC, r.httpSessionConn(sid, s, "client"))
}

func (r *Relay) handleHDialToWS(w http.ResponseWriter, targetKey string, auth sessionauth.Open) {
	r.mu.Lock()
	ac := r.agents[targetKey]
	r.mu.Unlock()
	if ac == nil {
		http.Error(w, "device offline", http.StatusNotFound)
		return
	}

	sid := newID()
	s := r.newHTTPSession(sid)
	ps := &pendingSession{agentSide: make(chan io.ReadWriteCloser, 1), done: make(chan struct{})}
	r.mu.Lock()
	r.pending[sid] = ps
	r.mu.Unlock()

	auth.Op = "open"
	auth.Session = sid
	auth.URL = "/session/" + sid
	if err := ac.send(auth); err != nil {
		r.finishPendingSession(sid, ps)
		r.closeHTTPSession(sid, s)
		http.Error(w, "agent unreachable", http.StatusBadGateway)
		return
	}
	go r.bridgeHTTPControllerToWS(sid, s, ps)
	if r.audit != nil {
		r.audit.Audit(auth.OwnerNamespace, auth.Device, "dial")
	}
	writeJSON(w, map[string]string{"session": sid})
}

func (r *Relay) bridgeHTTPControllerToWS(sid string, s *httpSession, ps *pendingSession) {
	defer r.finishPendingSession(sid, ps)
	timer := time.NewTimer(agentSessionOpenTimeout)
	defer timer.Stop()
	select {
	case agentNC := <-ps.agentSide:
		pipe(r.httpSessionConn(sid, s, "agent"), agentNC)
	case <-timer.C:
		r.closeHTTPSession(sid, s)
	case <-s.toClient.done:
	}
}

func (r *Relay) finishPendingSession(sid string, ps *pendingSession) {
	close(ps.done)
	r.mu.Lock()
	if r.pending[sid] == ps {
		delete(r.pending, sid)
	}
	r.mu.Unlock()
}

func (r *Relay) liveDevices(ns string) []string {
	devices := map[string]struct{}{}
	r.mu.Lock()
	for _, a := range r.agents {
		if a.ns == ns {
			devices[a.device] = struct{}{}
		}
	}
	r.mu.Unlock()

	now := time.Now()
	r.hmu.Lock()
	for _, a := range r.hagents {
		if a.ns == ns && now.Sub(a.lastSeen) <= httpAgentTTL {
			devices[a.device] = struct{}{}
		}
	}
	r.hmu.Unlock()

	out := make([]string, 0, len(devices))
	for device := range devices {
		out = append(out, device)
	}
	sort.Strings(out)
	return out
}
