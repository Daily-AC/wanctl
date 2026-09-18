package relay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"wanctl/internal/delegation"
	"wanctl/internal/httpconn"
	"wanctl/internal/limits"
	"wanctl/internal/sessionauth"
)

// httpAgent is an online device reachable over the HTTP transport. The agent
// keeps a long-poll on /h/poll; the relay pushes session ids to open onto `open`.
type httpAgent struct {
	ns, device, name string
	open             chan sessionauth.Open
	lastSeen         time.Time
	inst             string
	retired          map[string]struct{}
	changed          chan struct{}
	delegation       bool
}

// sideQueue is one direction of a session's byte flow. The relay never inspects
// the bytes (they are end-to-end TLS). It is drained by long-poll /h/down GETs
// rather than a single streaming response, so it survives reverse proxies that
// buffer responses (e.g. thunderbox's nginx ignores X-Accel-Buffering).
type sideQueue struct {
	ch   chan []byte
	done chan struct{}
	once sync.Once

	// turn admits one poll at a time. Checking the ack, draining, assigning the
	// sequence and storing the unacked chunk have to be a single operation:
	// two polls that overlap — a reader whose request was cancelled while it
	// was parked on an empty queue, plus the retry it sent afterwards — would
	// otherwise each take a chunk, and the second would overwrite the first's
	// unacked chunk and lose it for good.
	turn chan struct{}

	// A drained chunk is removed from ch before it is written to an HTTP
	// response, so if that response is not delivered whole the bytes are gone
	// and the end-to-end TLS stream has a hole in it. Readers that speak the
	// acknowledged down protocol therefore get the chunk held here until they
	// report having received it (issue #57).
	ackMu   sync.Mutex
	seq     uint64
	unacked []byte
	// head is the tail of a chunk that was split at the drain cap. It is served
	// before anything still in ch, so splitting never reorders the stream.
	head []byte
	// inflight marks a poll that holds the turn and may be part way through
	// taking bytes out. Between the receive from ch and the store into unacked
	// those bytes are in no field at all, so without this the queue looks empty
	// while a whole chunk is in a poll's hands.
	inflight bool
}

func newSideQueue() *sideQueue {
	return &sideQueue{ch: make(chan []byte, 256), done: make(chan struct{}), turn: make(chan struct{}, 1)}
}

// push enqueues a copy of b, or reports false once the queue is closed. The
// decision is taken under ackMu, which close also takes, so "is it closed" and
// "enqueue it" cannot both look true to a writer racing a close: once close has
// returned, every later push is refused. Only a push that finds the queue full
// waits outside the lock, and one that was already waiting there when the close
// landed is genuinely concurrent with it, so either answer is honest.
func (q *sideQueue) push(b []byte) bool {
	cp := make([]byte, len(b))
	copy(cp, b)
	q.ackMu.Lock()
	select {
	case <-q.done:
		q.ackMu.Unlock()
		return false
	default:
	}
	select {
	case q.ch <- cp:
		q.ackMu.Unlock()
		return true
	default:
	}
	q.ackMu.Unlock()
	select {
	case q.ch <- cp:
		return true
	case <-q.done:
		return false
	}
}

func (q *sideQueue) close() {
	q.once.Do(func() {
		q.ackMu.Lock()
		close(q.done)
		q.ackMu.Unlock()
	})
}

// beginTake and endTake bracket a poll's hold on the queue, so that a chunk
// which has left ch but not yet reached unacked still counts as being here.
// They are the same critical section unacked is published in, which is what
// makes settled's answer good until the queue is next touched.
func (q *sideQueue) beginTake() {
	q.ackMu.Lock()
	q.inflight = true
	q.ackMu.Unlock()
}

func (q *sideQueue) endTake() {
	q.ackMu.Lock()
	q.inflight = false
	q.ackMu.Unlock()
}

// settled reports that this direction can never hand anyone another byte: it is
// closed to new ones, no poll is part way through taking any out, and none are
// left queued, held as a split tail, or waiting to be acknowledged.
//
// The closed check is what keeps the answer from going stale. Once it holds,
// push refuses and only a take could move anything — and a take can only find
// what the other three checks just said is not there.
func (q *sideQueue) settled() bool {
	select {
	case <-q.done:
	default:
		return false
	}
	q.ackMu.Lock()
	defer q.ackMu.Unlock()
	return !q.inflight && q.unacked == nil && q.head == nil && len(q.ch) == 0
}

// acquire admits this poll, waiting for any poll already in flight on this
// direction to finish. It reports false when the caller's request went away
// first, in which case nothing was taken from the queue.
func (q *sideQueue) acquire(ctx context.Context) bool {
	// A free turn is always taken, even by a request that has already been
	// abandoned: it still has to record whatever it drains as unacked rather
	// than leave the queue to a poll that would renumber it.
	select {
	case q.turn <- struct{}{}:
		return true
	default:
	}
	select {
	case q.turn <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (q *sideQueue) release() { <-q.turn }

// take is drain for a reader that acknowledges what it received. ack is the
// highest sequence the reader has fully received. While an older chunk is still
// outstanding it is returned again, byte for byte, under its original sequence;
// only an ack that covers it lets the relay move on. The returned seq is 0 when
// there is no data, and ok is false when the request was abandoned before this
// poll got its turn.
//
// A chunk is recorded as unacked before take returns, so a request whose
// context is cancelled after the drain — the response never reaching the reader
// — leaves the bytes to be re-served to the next poll rather than dropping them.
func (q *sideQueue) take(ctx context.Context, ack uint64, timeout time.Duration) (data []byte, seq uint64, closed, ok bool) {
	if !q.acquire(ctx) {
		return nil, 0, false, false
	}
	defer q.release()
	q.beginTake()
	defer q.endTake() // runs after unacked is stored, before the turn is freed

	q.ackMu.Lock()
	if q.unacked != nil {
		if ack < q.seq {
			data, seq = q.unacked, q.seq
			q.ackMu.Unlock()
			return data, seq, false, true
		}
		q.unacked = nil
	}
	q.ackMu.Unlock()

	data, closed = q.drain(ctx, timeout)
	if len(data) == 0 {
		return nil, 0, closed, true
	}
	q.ackMu.Lock()
	q.seq++
	seq = q.seq
	q.unacked = data
	q.ackMu.Unlock()
	return data, seq, false, true
}

// pollDrain serves a reader that cannot acknowledge: a pre-acknowledgement
// client, or the in-process bridge, where the hand-off is a function return
// rather than a response that can half arrive. It takes the same turn as take
// so two readers never drain the same direction at once.
func (q *sideQueue) pollDrain(ctx context.Context, timeout time.Duration) (data []byte, closed, ok bool) {
	if !q.acquire(ctx) {
		return nil, false, false
	}
	defer q.release()
	q.beginTake()
	defer q.endTake()
	data, closed = q.drain(ctx, timeout)
	return data, closed, true
}

// maxDrainBytes is a hard cap on how much one drain coalesces into a single
// down-poll response. Uncapped, a reader that fell behind is handed everything
// the writer queued meanwhile — megabytes in one response — and on issue #57's
// 60 KB/s link a response that large could not finish downloading inside the
// client's request bound, so it was cut in half and the TLS stream lost a chunk
// out of its middle.
//
// The size is chosen against that link. 2 MiB at 60 KB/s takes about 34 s to
// download, and the client bounds one request at 5 minutes, so a full response
// finishes in roughly a ninth of its budget even after the 20 s the poll may
// have parked on the relay first. Larger buys nothing there and only makes the
// re-send after a truncated body more expensive; smaller costs throughput
// everywhere else, because serial polling cannot carry more than maxDrainBytes
// per round trip (2 MiB / 100 ms RTT = 20 MiB/s, against 256 KiB / 100 ms RTT
// = 2.5 MiB/s).
//
// It bounds the response because a chunk that would overshoot is split and its
// tail served first next time, not appended whole. The memory one direction
// holds is therefore maxDrainBytes for the unacked chunk, plus the tail of at
// most one split chunk, plus the 256-slot queue itself — whose chunks are
// bounded by limits.RelayHTTPUploadBytes on the /h/up path but not on the
// in-process bridge, which is why the split has to exist at all.
const maxDrainBytes = 2 << 20

// drain returns bytes available within timeout, coalescing queued chunks up to
// maxDrainBytes. closed is true only when the queue is closed and no more bytes
// remain. Callers hold the queue's turn.
func (q *sideQueue) drain(ctx context.Context, timeout time.Duration) (data []byte, closed bool) {
	var out []byte
	// appendCapped takes as much of b as still fits and parks the rest in head.
	// out is grown by hand so that neither its length nor the memory behind it
	// can pass the cap, and an idle poll that never sees a byte allocates none.
	appendCapped := func(b []byte) {
		if room := maxDrainBytes - len(out); len(b) > room {
			q.ackMu.Lock()
			q.head = b[room:]
			q.ackMu.Unlock()
			b = b[:room]
		}
		if need := len(out) + len(b); need > cap(out) {
			grown := make([]byte, len(out), max(need, min(2*cap(out), maxDrainBytes)))
			copy(grown, out)
			out = grown
		}
		out = append(out, b...)
	}
	// fill drains what is already queued, without waiting.
	fill := func() {
		for len(out) < maxDrainBytes {
			select {
			case b := <-q.ch:
				appendCapped(b)
			default:
				return
			}
		}
	}
	q.ackMu.Lock()
	head := q.head
	q.head = nil
	q.ackMu.Unlock()
	if len(head) > 0 {
		appendCapped(head)
	}
	fill()
	if len(out) > 0 {
		return out, false
	}
	select {
	case b := <-q.ch:
		appendCapped(b)
		fill()
		return out, false
	case <-time.After(timeout):
		return nil, false
	case <-ctx.Done():
		return nil, false
	case <-q.done:
		select {
		case b := <-q.ch:
			appendCapped(b)
			fill()
			return out, false
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
	callerNS     string
	credentialID string
	lease        *accessLease
	ownerNS      string
	lastActive   time.Time
	// closedAt is set when a peer closed the session gracefully. The session
	// then stops accepting new bytes but stays in the registry, so the peer
	// still reading it collects what is already queued instead of having the
	// remainder 404 out from under it. Guarded by hmu.
	closedAt time.Time
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
	created := false
	deviceID := req.URL.Query().Get("device_id")
	key := ns + "/" + device
	inst := req.URL.Query().Get("inst")
	r.startHTTPReaper()
	r.registrationMu.Lock()
	r.hmu.Lock()
	a := r.hagents[key]
	wasHTTPLive := a != nil && time.Since(a.lastSeen) <= httpAgentTTL
	if a == nil {
		if r.countNamespaceAgentsLocked(ns) >= httpAgentsPerNS {
			r.hmu.Unlock()
			r.registrationMu.Unlock()
			http.Error(w, "too many devices registered for this namespace", 429)
			return
		}
		a = &httpAgent{ns: ns, device: device, open: make(chan sessionauth.Open, 8), changed: make(chan struct{})}
	}
	if _, old := a.retired[inst]; inst != "" && old {
		r.hmu.Unlock()
		r.registrationMu.Unlock()
		http.Error(w, "another agent instance registered this device ID", 409)
		return
	}
	r.hmu.Unlock()
	var err error
	if deviceID != "" {
		if deviceID != device {
			r.registrationMu.Unlock()
			http.Error(w, "device ID mismatch", 400)
			return
		}
		created, err = r.registerDeviceID(ns, deviceID, req.URL.Query().Get("name"), req.URL.Query().Get("fp"))
	} else {
		err = r.allowLegacyRegistration(ns, device)
		if err == nil {
			created = r.recordDeviceRegistration(ns, device, req.URL.Query().Get("fp"))
		}
	}
	if err != nil {
		r.registrationMu.Unlock()
		http.Error(w, "device registration failed", 409)
		return
	}
	r.hmu.Lock()
	if inst != "" {
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
	a.name = req.URL.Query().Get("name")
	a.delegation = req.URL.Query().Get("delegation") == "1"
	a.lastSeen = time.Now()
	r.hagents[key] = a
	changed := a.changed
	r.hmu.Unlock()
	r.registrationMu.Unlock()
	wasLive := wasHTTPLive || r.wsDeviceLive(key)
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
	access, token, ok := r.authAccess(w, req)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// HTTP controllers can dial a WebSocket agent without any HTTP agent ever
	// polling. Start the session reaper on this path too, or abandoned hybrid
	// sessions live forever despite the idle deadline.
	r.startHTTPReaper()
	targetKey, auth, reason, ok := r.dialAccessAllowed(access, req.URL.Query().Get("target"))
	if !ok {
		http.Error(w, dialRefusal(reason), http.StatusForbidden)
		return
	}
	r.hmu.Lock()
	a := r.hagents[targetKey]
	if a == nil || time.Since(a.lastSeen) > httpAgentTTL {
		r.hmu.Unlock()
		r.handleHDialToWS(w, targetKey, auth, access, token)
		return
	}
	if access.Delegated && !a.delegation {
		r.hmu.Unlock()
		http.Error(w, "device agent must be upgraded for delegated access", http.StatusConflict)
		return
	}
	sid := newID()
	auth.Session = sid
	r.hmu.Unlock()
	r.newHTTPSession(sid, auth, access, token)

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
	access, _, ok := r.authAccess(w, req)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, r.accessPeers(access))
}

func (r *Relay) handleHUp(w http.ResponseWriter, req *http.Request) {
	access, _, ok := r.authAccess(w, req)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s := r.sessionForAccess(req.URL.Query().Get("session"), access, req.URL.Query().Get("role"))
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
	if s.lease != nil && !s.lease.credentialValid() {
		r.closeHTTPSession(req.URL.Query().Get("session"), s)
		http.Error(w, "session closed", http.StatusGone)
		return
	}
	// A gracefully closed session is kept reachable so the far side can finish
	// reading it, not so it can be written to again. Refusing here is what the
	// queue would say anyway; saying it before the push keeps the answer the
	// same for an empty body, which never reaches the queue at all.
	if r.gracefullyClosed(s) {
		http.Error(w, "session closed", http.StatusGone)
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
	access, _, ok := r.authAccess(w, req)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// Tell the reader this relay honours ack=. It rides on every answer past
	// this point — the data-bearing 200, the empty 204, and the 400/404/410
	// refusals — but not on the 401 above, which is answered before the relay
	// knows who is asking. A reader may only retry a poll the carrier failed to
	// deliver once it has seen this, because a relay without it has already
	// dequeued the bytes and a retry would skip them.
	w.Header().Set(httpconn.DownAckCapabilityHeader, "1")
	s := r.sessionForAccess(req.URL.Query().Get("session"), access, req.URL.Query().Get("role"))
	if s == nil {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	src := s.toClient // role=client reads bytes destined for the client
	if req.URL.Query().Get("role") == "agent" {
		src = s.toAgent
	}
	// Whatever this poll does, it may be the one that empties the last
	// direction or releases the last hold on it, so it checks on the way out.
	// Reaching EOF is not the only way a session becomes finished, and a check
	// that ran too early is never retried unless every site retries it.
	defer r.releaseDrainedSession(req.URL.Query().Get("session"), s)
	var (
		data   []byte
		seq    uint64
		closed bool
		served bool
	)
	if ackParam := req.URL.Query().Get(httpconn.DownAckParam); ackParam != "" {
		ack, err := strconv.ParseUint(ackParam, 10, 64)
		if err != nil {
			http.Error(w, "bad ack", http.StatusBadRequest)
			return
		}
		data, seq, closed, served = src.take(req.Context(), ack, downPollWait)
	} else {
		// Pre-acknowledgement client: serve it the old fire-and-forget way so
		// a mixed-version fleet keeps working.
		data, closed, served = src.pollDrain(req.Context(), downPollWait)
	}
	if !served {
		return // the reader gave up before this poll got its turn
	}
	if s.lease != nil && !s.lease.credentialValid() {
		r.closeHTTPSession(req.URL.Query().Get("session"), s)
		http.Error(w, "session closed", http.StatusGone)
		return
	}
	if closed && len(data) == 0 {
		http.Error(w, "session closed", http.StatusGone)
		return
	}
	if len(data) == 0 {
		w.WriteHeader(http.StatusNoContent) // no data this round; client re-polls
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if seq > 0 {
		w.Header().Set(httpconn.DownSeqHeader, strconv.FormatUint(seq, 10))
	}
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func (r *Relay) handleHClose(w http.ResponseWriter, req *http.Request) {
	access, _, ok := r.authAccess(w, req)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sid := req.URL.Query().Get("session")
	r.hmu.Lock()
	s := r.hsess[sid]
	if s == nil || !s.allowsAccess(access, req.URL.Query().Get("role")) {
		r.hmu.Unlock()
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	s.closedAt = time.Now()
	r.hmu.Unlock()
	// Closing the queues stops new bytes and makes the far side see EOF once
	// they run dry, but the session stays registered: with the drain cap a
	// backlog needs several more polls to come out, and deleting it here would
	// 404 them away. It leaves once both directions have been taken, or when
	// the sweeper finds nobody polling it any more.
	s.close()
	// Closing the second queue can be the last thing a finished session was
	// waiting for, and a poll that woke on the first one has already looked.
	r.releaseDrainedSession(sid, s)
	w.WriteHeader(http.StatusOK)
}

// releaseDrainedSession retires a gracefully closed session once *both*
// directions have been taken. Reaching the end of one direction says nothing
// about the other: a controller that has read everything it was sent still
// leaves the agent holding an unacknowledged chunk and a split tail, and
// retiring the session on the first EOF would 404 those away — including a
// chunk a poll has already pulled out of the queue but not yet recorded, which
// is why settled covers a take in flight and not just the fields it writes. A
// session torn
// down any other way (credential revocation, dial failure, the idle sweeper) is
// already gone from the registry and this is a no-op.
//
// Every site that can make the last of those conditions true calls this
// afterwards — each poll, each read on the in-process bridge, and the close
// itself, which is what shuts the second queue. One call alone would not do:
// whoever looks first may look while another reader still has a chunk in hand,
// and a check that came too early is only harmless if someone checks again.
//
// When the far side never comes back to drain its direction, none of them can
// succeed and the session is retired by the idle sweeper instead: reapHTTP
// scans every httpAgentTTL and drops a session no one has polled for
// httpSessionIdle.
func (r *Relay) releaseDrainedSession(sid string, s *httpSession) {
	if !s.toClient.settled() || !s.toAgent.settled() {
		return
	}
	r.hmu.Lock()
	retire := !s.closedAt.IsZero() && r.hsess[sid] == s
	if retire {
		delete(r.hsess, sid)
	}
	r.hmu.Unlock()
	if retire && s.lease != nil {
		s.lease.close()
	}
}

// gracefullyClosed reports whether a peer has ended this session. Like
// closedAt itself it is guarded by hmu.
func (r *Relay) gracefullyClosed(s *httpSession) bool {
	r.hmu.Lock()
	defer r.hmu.Unlock()
	return !s.closedAt.IsZero()
}

func (r *Relay) session(sid string) *httpSession {
	r.hmu.Lock()
	defer r.hmu.Unlock()
	return r.hsess[sid]
}

// Delegated controllers can touch only their own session's client role. Full
// credentials preserve the existing owner/controller namespace behavior.
func (s *httpSession) allowsAccess(a delegation.Access, role string) bool {
	if a.Delegated {
		return (role == "" || role == "client") && s.credentialID != "" && a.CredentialID == s.credentialID && a.Namespace == s.callerNS
	}
	return a.Namespace == s.callerNS || a.Namespace == s.ownerNS
}

func (r *Relay) sessionForAccess(sid string, a delegation.Access, role string) *httpSession {
	r.hmu.Lock()
	defer r.hmu.Unlock()
	s := r.hsess[sid]
	if s == nil || !s.allowsAccess(a, role) {
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
// risk. This is also the bounded idle time that retires a gracefully closed
// session nobody came back to drain. Exported timing via the constants keeps
// the test honest.
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
		if s.lease != nil {
			s.lease.close()
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
