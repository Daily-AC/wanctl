package relay

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"wanctl/internal/limits"
	"wanctl/internal/sessionauth"
)

// httpAgent is an online device reachable over the HTTP transport. The agent
// keeps a long-poll on /h/poll; the relay pushes session ids to open onto `open`.
type httpAgent struct {
	ns, device string
	open       chan sessionauth.Open
	lastSeen   time.Time
	inst       string
	retired    map[string]struct{}
	changed    chan struct{}
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
	// The two namespaces allowed to touch this session's queues: the dialing
	// controller and the device owner. The session id is a 128-bit secret, but
	// binding the parties means a leaked id in one namespace cannot be used to
	// inject into or tear down a session in another (audit 2026-08-28,
	// SEC-A-02). lastActive is bumped by every /h/up and /h/down so the sweeper
	// can reap sessions that both parties have abandoned (SEC-A-03).
	callerNS   string
	ownerNS    string
	lastActive time.Time
}

func (s *httpSession) close() {
	s.toClient.close()
	s.toAgent.close()
}

const (
	httpAgentTTL = 40 * time.Second
	// httpAgentsPerNS bounds how many distinct device names one namespace may
	// hold in the HTTP registry at once. Without it a single token could poll
	// /h/poll?device=<unique> in a loop and grow the registry without bound —
	// one client took the relay from 21 MB to 700 MB in a local run (audit
	// 2026-08-28, SEC-A-01). Real namespaces have a handful of devices; this
	// only stops the flood, and only new names past the cap (existing devices
	// keep polling).
	httpAgentsPerNS = 256
	// httpSessionIdle reaps a session neither party has polled for this long.
	// A live session is polled at least every downPollWait (20s); an abandoned
	// one is not, so 3× is comfortably clear of a slow but live command.
	httpSessionIdle = 3 * downPollWait
	downPollWait    = 20 * time.Second
)

func (r *Relay) handleHPoll(w http.ResponseWriter, req *http.Request) {
	ns, ok := r.auth(w, req)
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
	inst := req.URL.Query().Get("inst")
	r.startHTTPReaper()
	r.hmu.Lock()
	a := r.hagents[key]
	wasHTTPLive := a != nil && time.Since(a.lastSeen) <= httpAgentTTL
	if a == nil {
		if r.countNamespaceAgentsLocked(ns) >= httpAgentsPerNS {
			r.hmu.Unlock()
			http.Error(w, "too many devices registered for this namespace", http.StatusTooManyRequests)
			return
		}
		a = &httpAgent{ns: ns, device: device, open: make(chan sessionauth.Open, 8), changed: make(chan struct{})}
		r.hagents[key] = a
	}
	if inst != "" {
		if _, old := a.retired[inst]; old {
			r.hmu.Unlock()
			http.Error(w, "another agent instance registered this device name", http.StatusConflict)
			return
		}
		if a.inst != "" && a.inst != inst {
			if a.retired == nil {
				a.retired = map[string]struct{}{}
			}
			a.retired[a.inst] = struct{}{}
			close(a.changed)
			a.changed = make(chan struct{})
		}
		a.inst = inst
	}
	a.lastSeen = time.Now()
	changed := a.changed
	r.hmu.Unlock()
	wasLive := wasHTTPLive || r.wsDeviceLive(key)
	created := r.recordDeviceRegistration(ns, device, req.URL.Query().Get("fp"))
	if !wasLive {
		r.emitDeviceEvent(ns, device, onlineEvent(device))
	}
	if created {
		r.emitAccountEvent(ns, enrollEvent(device))
	}

	select {
	case open := <-a.open:
		if inst != "" && r.httpAgentObsolete(key, inst) {
			r.requeueHTTPJob(key, open)
			http.Error(w, "another agent instance registered this device name", http.StatusConflict)
			return
		}
		writeJSON(w, open)
	case <-changed:
		if inst != "" && r.httpAgentObsolete(key, inst) {
			http.Error(w, "another agent instance registered this device name", http.StatusConflict)
		}
	case <-time.After(25 * time.Second):
		writeJSON(w, map[string]string{})
	case <-req.Context().Done():
	}
}

func (r *Relay) httpAgentObsolete(key, inst string) bool {
	r.hmu.Lock()
	defer r.hmu.Unlock()
	a := r.hagents[key]
	if a == nil || inst == "" {
		return false
	}
	if _, old := a.retired[inst]; old {
		return true
	}
	return a.inst != "" && a.inst != inst
}

func (r *Relay) requeueHTTPJob(key string, open sessionauth.Open) {
	r.hmu.Lock()
	a := r.hagents[key]
	r.hmu.Unlock()
	if a == nil {
		return
	}
	a.open <- open
}

func (r *Relay) handleHDial(w http.ResponseWriter, req *http.Request) {
	ns, ok := r.auth(w, req)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// HTTP controllers can dial a WebSocket agent without any HTTP agent ever
	// polling. Start the session reaper on this path too, or abandoned hybrid
	// sessions live forever despite the idle deadline.
	r.startHTTPReaper()
	targetKey, auth, ok := r.dialAllowed(ns, req.URL.Query().Get("target"))
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	r.hmu.Lock()
	a := r.hagents[targetKey]
	if a == nil || time.Since(a.lastSeen) > httpAgentTTL {
		r.hmu.Unlock()
		r.handleHDialToWS(w, targetKey, auth)
		return
	}
	sid := newID()
	auth.Session = sid
	r.hmu.Unlock()
	r.newHTTPSession(sid, auth)

	select {
	case a.open <- auth:
	default:
		r.closeHTTPSession(sid, r.session(sid))
		http.Error(w, "agent busy", http.StatusServiceUnavailable)
		return
	}
	if r.audit != nil {
		r.audit.Audit(auth.OwnerNamespace, auth.Device, "dial")
	}
	writeJSON(w, map[string]string{"session": sid})
}

// handleHDeregister lets an agent announce it is going offline now, so the relay
// drops it from the live registry immediately (no TTL wait).
func (r *Relay) handleHDeregister(w http.ResponseWriter, req *http.Request) {
	ns, ok := r.auth(w, req)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	device := req.URL.Query().Get("device")
	key := ns + "/" + device
	inst := req.URL.Query().Get("inst")
	r.hmu.Lock()
	removed := false
	if inst != "" {
		if a := r.hagents[key]; a != nil && a.inst != "" && a.inst != inst {
			r.hmu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
	}
	if _, ok := r.hagents[key]; ok {
		delete(r.hagents, key)
		removed = true
	}
	r.hmu.Unlock()
	if removed && !r.deviceLive(ns, device) {
		r.emitDeviceEvent(ns, device, offlineEvent(device))
	}
	if r.audit != nil {
		r.audit.Audit(ns, device, "deregister")
	}
	w.WriteHeader(http.StatusOK)
}

// deviceLive reports whether a device currently holds a live control channel —
// an HTTP long-poll within the TTL, or a connected WebSocket. This is the
// dial-able truth, unlike the DB's lagging last_seen.
func (r *Relay) deviceLive(ns, device string) bool {
	key := ns + "/" + device
	r.hmu.Lock()
	if a := r.hagents[key]; a != nil && time.Since(a.lastSeen) <= httpAgentTTL {
		r.hmu.Unlock()
		return true
	}
	r.hmu.Unlock()
	return r.wsDeviceLive(key)
}

func (r *Relay) wsDeviceLive(key string) bool {
	r.mu.Lock()
	_, live := r.agents[key]
	r.mu.Unlock()
	return live
}

func (r *Relay) handleHPeers(w http.ResponseWriter, req *http.Request) {
	ns, ok := r.auth(w, req)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, map[string]any{"namespace": ns, "devices": r.liveDevices(ns)})
}

func (r *Relay) handleHUp(w http.ResponseWriter, req *http.Request) {
	ns, ok := r.auth(w, req)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s := r.sessionForParty(req.URL.Query().Get("session"), ns)
	if s == nil {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, limits.RelayHTTPUploadBytes)
	body, err := io.ReadAll(req.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
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
	ns, ok := r.auth(w, req)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s := r.sessionForParty(req.URL.Query().Get("session"), ns)
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
	ns, ok := r.auth(w, req)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sid := req.URL.Query().Get("session")
	r.hmu.Lock()
	s := r.hsess[sid]
	if s == nil || (ns != s.callerNS && ns != s.ownerNS) {
		r.hmu.Unlock()
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	delete(r.hsess, sid)
	r.hmu.Unlock()
	s.close()
	w.WriteHeader(http.StatusOK)
}

func (r *Relay) session(sid string) *httpSession {
	r.hmu.Lock()
	defer r.hmu.Unlock()
	return r.hsess[sid]
}

// sessionForParty returns the session only if ns is one of its two parties
// (the dialing controller or the device owner), and marks it active. A caller
// that is neither gets nil — indistinguishable from an unknown id.
func (r *Relay) sessionForParty(sid, ns string) *httpSession {
	r.hmu.Lock()
	defer r.hmu.Unlock()
	s := r.hsess[sid]
	if s == nil || (ns != s.callerNS && ns != s.ownerNS) {
		return nil
	}
	s.lastActive = time.Now()
	return s
}

// countNamespaceAgentsLocked counts distinct devices ns holds in the HTTP
// registry. Caller holds hmu.
func (r *Relay) countNamespaceAgentsLocked(ns string) int {
	n := 0
	for _, a := range r.hagents {
		if a.ns == ns {
			n++
		}
	}
	return n
}

// startHTTPReaper launches the registry/session sweeper once, on the first
// HTTP poll a relay ever serves (a WS-only or env-token smoke relay never
// spawns it).
func (r *Relay) startHTTPReaper() {
	r.reaperOnce.Do(func() {
		go func() {
			t := time.NewTicker(httpAgentTTL)
			defer t.Stop()
			for range t.C {
				r.reapHTTP(time.Now())
			}
		}()
	})
}

// reapHTTP drops HTTP-registry entries whose agent stopped polling and
// sessions both parties abandoned. A live agent refreshes lastSeen every poll
// cycle and a live session is polled every downPollWait, so neither is at
// risk. Exported timing via the constants keeps the test honest.
func (r *Relay) reapHTTP(now time.Time) {
	var dead []*httpSession
	var offline []*httpAgent
	r.hmu.Lock()
	for key, a := range r.hagents {
		if now.Sub(a.lastSeen) > httpAgentTTL {
			delete(r.hagents, key)
			offline = append(offline, a)
		}
	}
	for sid, s := range r.hsess {
		if !s.lastActive.IsZero() && now.Sub(s.lastActive) > httpSessionIdle {
			delete(r.hsess, sid)
			dead = append(dead, s)
		}
	}
	r.hmu.Unlock()
	for _, a := range offline {
		if !r.wsDeviceLive(a.ns + "/" + a.device) {
			r.emitDeviceEvent(a.ns, a.device, offlineEvent(a.device))
		}
	}
	for _, s := range dead {
		s.close()
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
