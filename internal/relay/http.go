package relay

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// httpAgent is an online device reachable over the HTTP transport. The agent
// keeps a long-poll on /h/poll; the relay pushes session ids to open onto `open`.
type httpAgent struct {
	ns, device string
	open       chan string
	lastSeen   time.Time
}

// sideQueue is one direction of a session's byte flow. The relay never inspects
// the bytes (they are end-to-end TLS). It is drained by long-poll /h/down GETs
// rather than a single streaming response, so it survives reverse proxies that
// buffer responses (e.g. thunderbox's nginx ignores X-Accel-Buffering).
type sideQueue struct {
	ch   chan []byte
	done chan struct{}
	once sync.Once
}

func newSideQueue() *sideQueue {
	return &sideQueue{ch: make(chan []byte, 256), done: make(chan struct{})}
}

func (q *sideQueue) push(b []byte) bool {
	cp := make([]byte, len(b))
	copy(cp, b)
	select {
	case q.ch <- cp:
		return true
	case <-q.done:
		return false
	}
}

func (q *sideQueue) close() { q.once.Do(func() { close(q.done) }) }

// drain returns bytes available within timeout, coalescing any queued chunks.
// closed is true only when the queue is closed and no more bytes remain.
func (q *sideQueue) drain(timeout time.Duration) (data []byte, closed bool) {
	collect := func(first []byte) []byte {
		out := append([]byte{}, first...)
		for {
			select {
			case b := <-q.ch:
				out = append(out, b...)
			default:
				return out
			}
		}
	}
	select {
	case b := <-q.ch:
		return collect(b), false
	default:
	}
	select {
	case b := <-q.ch:
		return collect(b), false
	case <-time.After(timeout):
		return nil, false
	case <-q.done:
		select {
		case b := <-q.ch:
			return collect(b), false
		default:
			return nil, true
		}
	}
}

// httpSession carries the two directions of a relayed session.
type httpSession struct {
	toClient *sideQueue // bytes the controller will read
	toAgent  *sideQueue // bytes the agent will read
}

const (
	httpAgentTTL = 40 * time.Second
	downPollWait = 20 * time.Second
)

func (r *Relay) handleHPoll(w http.ResponseWriter, req *http.Request) {
	ns, ok := r.auth(req)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	device := req.URL.Query().Get("device")
	if device == "" {
		http.Error(w, "device required", http.StatusBadRequest)
		return
	}
	key := ns + "/" + device
	r.hmu.Lock()
	a := r.hagents[key]
	if a == nil {
		a = &httpAgent{ns: ns, device: device, open: make(chan string, 8)}
		r.hagents[key] = a
	}
	a.lastSeen = time.Now()
	r.hmu.Unlock()

	select {
	case sid := <-a.open:
		writeJSON(w, map[string]string{"session": sid})
	case <-time.After(25 * time.Second):
		writeJSON(w, map[string]string{})
	case <-req.Context().Done():
	}
}

func (r *Relay) handleHDial(w http.ResponseWriter, req *http.Request) {
	ns, ok := r.auth(req)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	target := req.URL.Query().Get("target")
	if !strings.Contains(target, "/") {
		target = ns + "/" + target
	}
	// Foundation milestone: only same-namespace access (ACL is a later milestone).
	if !strings.HasPrefix(target, ns+"/") {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	r.hmu.Lock()
	a := r.hagents[target]
	if a == nil || time.Since(a.lastSeen) > httpAgentTTL {
		r.hmu.Unlock()
		http.Error(w, "device offline", http.StatusNotFound)
		return
	}
	sid := newID()
	r.hsess[sid] = &httpSession{toClient: newSideQueue(), toAgent: newSideQueue()}
	r.hmu.Unlock()

	select {
	case a.open <- sid:
	default:
		r.hmu.Lock()
		delete(r.hsess, sid)
		r.hmu.Unlock()
		http.Error(w, "agent busy", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]string{"session": sid})
}

func (r *Relay) handleHPeers(w http.ResponseWriter, req *http.Request) {
	ns, ok := r.auth(req)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	devices := []string{}
	r.hmu.Lock()
	for _, a := range r.hagents {
		if a.ns == ns && time.Since(a.lastSeen) <= httpAgentTTL {
			devices = append(devices, a.device)
		}
	}
	r.hmu.Unlock()
	writeJSON(w, map[string]any{"namespace": ns, "devices": devices})
}

func (r *Relay) handleHUp(w http.ResponseWriter, req *http.Request) {
	if _, ok := r.auth(req); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s := r.session(req.URL.Query().Get("session"))
	if s == nil {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	dst := s.toAgent // role=client writes toward the agent
	if req.URL.Query().Get("role") == "agent" {
		dst = s.toClient
	}
	if len(body) > 0 && !dst.push(body) {
		http.Error(w, "session closed", http.StatusGone)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (r *Relay) handleHDown(w http.ResponseWriter, req *http.Request) {
	if _, ok := r.auth(req); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s := r.session(req.URL.Query().Get("session"))
	if s == nil {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	src := s.toClient // role=client reads bytes destined for the client
	if req.URL.Query().Get("role") == "agent" {
		src = s.toAgent
	}
	data, closed := src.drain(downPollWait)
	if closed && len(data) == 0 {
		http.Error(w, "session closed", http.StatusGone)
		return
	}
	if len(data) == 0 {
		w.WriteHeader(http.StatusNoContent) // no data this round; client re-polls
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func (r *Relay) handleHClose(w http.ResponseWriter, req *http.Request) {
	if _, ok := r.auth(req); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sid := req.URL.Query().Get("session")
	r.hmu.Lock()
	s := r.hsess[sid]
	delete(r.hsess, sid)
	r.hmu.Unlock()
	if s != nil {
		s.toClient.close()
		s.toAgent.close()
	}
	w.WriteHeader(http.StatusOK)
}

func (r *Relay) session(sid string) *httpSession {
	r.hmu.Lock()
	defer r.hmu.Unlock()
	return r.hsess[sid]
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
