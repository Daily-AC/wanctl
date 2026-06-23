package relay

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// httpAgent is an online device reachable over the HTTP transport. The agent
// keeps a long-poll on /h/poll; the relay pushes session ids to open onto `open`.
type httpAgent struct {
	ns, device string
	open       chan string
	lastSeen   time.Time
}

// httpSession is one relayed session. Two io.Pipes carry the two directions; the
// relay never inspects the bytes (they are end-to-end TLS).
type httpSession struct {
	toClientR, toAgentR *io.PipeReader
	toClientW, toAgentW *io.PipeWriter
}

const httpAgentTTL = 40 * time.Second

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
	tcR, tcW := io.Pipe()
	taR, taW := io.Pipe()
	r.hsess[sid] = &httpSession{toClientR: tcR, toClientW: tcW, toAgentR: taR, toAgentW: taW}
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
	dst := s.toAgentW // role=client writes toward the agent
	if req.URL.Query().Get("role") == "agent" {
		dst = s.toClientW
	}
	if _, err := io.Copy(dst, req.Body); err != nil {
		http.Error(w, "stream closed", http.StatusGone)
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
	src := s.toClientR // role=client reads bytes destined for the client
	if req.URL.Query().Get("role") == "agent" {
		src = s.toAgentR
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Accel-Buffering", "no") // ask nginx not to buffer this stream
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
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
		s.toClientW.Close()
		s.toAgentW.Close()
		s.toClientR.Close()
		s.toAgentR.Close()
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
