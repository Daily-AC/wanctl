package relay

import (
	"bytes"
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

	// turn serializes chunk assignment. Checking the ack, draining, assigning
	// the sequence and storing the chunk have to be a single operation:
	// two polls that overlap — a reader whose request was cancelled while it
	// was parked on an empty queue, plus the retry it sent afterwards — would
	// otherwise each take a chunk and lose their shared ordering.
	turn chan struct{}

	// A drained chunk is removed from ch before it is written to an HTTP
	// response, so if that response is not delivered whole the bytes are gone
	// and the end-to-end TLS stream has a hole in it. Readers that speak the
	// acknowledged down protocol therefore get the chunk held here until they
	// report having received it (issue #57).
	ackMu    sync.Mutex
	seq      uint64
	assigned map[uint64][]byte
	changed  chan struct{}
	unacked  []byte // mirrors the oldest assigned chunk for existing queue diagnostics
	// head is the tail of a chunk that was split at the drain cap. It is served
	// before anything still in ch, so splitting never reorders the stream.
	head []byte
	// seqMu orders the writes of a writer that keeps several /h/up in flight
	// at once. Those requests can arrive in any order, so each carries its
	// place in the stream and is held here until the ones before it land. A
	// sequence already delivered is a retry and is acknowledged without being
	// queued again, which is what makes resending an /h/up safe at all.
	seqMu   sync.Mutex
	seqNext uint64 // the sequence expected next; sequences start at 1
	seqHeld map[uint64][]byte
	// inflight marks a poll that holds the turn and may be part way through
	// taking bytes out. Between the receive from ch and the store into assigned
	// those bytes are in no field at all, so without this the queue looks empty
	// while a whole chunk is in a poll's hands.
	inflight bool

	// What this direction holds in memory, guarded by ackMu (see resident.go).
	// resident is every byte it holds or has promised room to: queued in ch or
	// head, waiting in seqHeld, assigned to a reader and not yet acknowledged,
	// and reserved for a write whose body is still being read. slots counts the
	// chunks in ch or seqHeld and the writes promised a place in ch, which is
	// what lets an admitted write be enqueued without ever waiting on ch.
	limit    int64
	resident int64
	slots    int
	room     chan struct{} // closed when resident or slots drop, if a writer waits
	pool     *residency    // the relay's account; nil for a queue outside one
	share    *nsShare      // the dialing namespace's part of pool; nil if exempt
	freed    bool          // free gave everything back; nothing is counted after it
}

// queueChunks is how many chunks one direction holds between ch, seqHeld and
// the writes admitted into it. The byte budget alone would let a writer of
// one-byte chunks pay many times their size in per-chunk overhead.
const queueChunks = 256

func newSideQueue() *sideQueue {
	return &sideQueue{ch: make(chan []byte, queueChunks), done: make(chan struct{}), turn: make(chan struct{}, 1), seqNext: 1, assigned: make(map[uint64][]byte), changed: make(chan struct{}), limit: maxResidentPerDirection}
}

// attach charges this direction to the relay's account, and to namespace ns's
// share of it unless ns is "".
func (q *sideQueue) attach(pool *residency, ns string) {
	q.pool = pool
	if ns != "" {
		q.share = pool.join(ns)
	}
}

// maxSeqAhead bounds how far past the next expected write a sequenced /h/up may
// be. A writer keeps eight in flight, but when one of them is retried, or just
// slow to get one of the writer's connections, the writer goes on posting the
// ones after it — dozens on a fast link — and each is held here until the gap
// fills. A write past the window is sent back to be tried again later (see
// acceptUpload). One inside it has to be admissible before the gap fills, or it
// would wait for room on one of the writer's connections, and with four of
// them waiting the write that fills the gap has no connection left to go out
// on. So the window is what one direction's budget holds in the largest
// writes, less the room kept for the missing write and one more to spare; a
// namespace's share has the same room left with the session's other direction
// full. seqWindowLocked works it out for a direction's actual budget.
const maxSeqAhead = uint64(maxResidentPerDirection/maxUploadBytes - 2)

// seqWindowLocked is maxSeqAhead for this direction's budget. Caller holds
// ackMu.
func (q *sideQueue) seqWindowLocked() uint64 {
	return uint64(max(q.limit/maxUploadBytes-2, 1))
}

var (
	errQueueClosed = errors.New("session closed")
	// errRepeatWrite is a write already taken, retried by a writer that did not
	// see the answer. It is acknowledged and not taken again.
	errRepeatWrite = errors.New("write already queued")
	errOutOfWindow = errors.New("write out of window")
)

// admitSeq waits until write number seq of n bytes may be read and holds room
// for it (see reserveLocked); commitSeq then takes it into the stream, or
// unreserve gives the room back. It returns errRepeatWrite for a write already
// taken, errOutOfWindow for one past the window (see maxSeqAhead),
// errQueueClosed once the queue has closed, and the context's error if the
// writer gave up waiting.
func (q *sideQueue) admitSeq(ctx context.Context, seq uint64, n int64) error {
	waited := false
	for {
		q.seqMu.Lock()
		next := q.seqNext
		_, held := q.seqHeld[seq]
		q.ackMu.Lock()
		window := q.seqWindowLocked()
		q.ackMu.Unlock()
		q.seqMu.Unlock()
		switch {
		case seq < next || held:
			return errRepeatWrite
		case seq > next+window:
			return errOutOfWindow
		}
		wait, err := q.tryReserve(n, seq > next, &waited)
		if err != nil || wait == nil {
			return err
		}
		if err := q.await(ctx, wait); err != nil {
			return err
		}
	}
}

// reserve is admitSeq for a writer that does not number its writes.
func (q *sideQueue) reserve(ctx context.Context, n int64) error {
	waited := false
	for {
		wait, err := q.tryReserve(n, false, &waited)
		if err != nil || wait == nil {
			return err
		}
		if err := q.await(ctx, wait); err != nil {
			return err
		}
	}
}

// await waits for room to be given back, the queue to close, or ctx to end.
// The caller holds no lock, so nothing a reader needs is held while it waits.
func (q *sideQueue) await(ctx context.Context, room <-chan struct{}) error {
	select {
	case <-room:
		return nil
	case <-q.done:
		return errQueueClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// tryReserve holds room for a write of n bytes, or returns the channel to wait
// on before trying again. waited records, across the tries of one write,
// whether it has already been counted as waiting on the relay's account.
func (q *sideQueue) tryReserve(n int64, ahead bool, waited *bool) (<-chan struct{}, error) {
	q.ackMu.Lock()
	defer q.ackMu.Unlock()
	select {
	case <-q.done:
		return nil, errQueueClosed
	default:
	}
	if n == 0 {
		return nil, nil // an empty write holds nothing and never enters ch
	}
	wait, onPool, nsFull := q.reserveLocked(n, ahead)
	if onPool && !*waited {
		*waited = true
		q.pool.noteWait(nsFull)
	}
	return wait, nil
}

// reserveLocked holds n bytes and one slot for a write, if this direction and
// the relay's account have room for them. A write that is ahead of the stream
// leaves room for the largest write free at every level: otherwise writes
// waiting on a gap could fill the budget and leave the write that fills the gap
// no room to land in. When there is no room it returns the channel to wait on,
// whether it was the relay's account rather than this direction that was full,
// and whether that was the namespace's share. Caller holds ackMu.
func (q *sideQueue) reserveLocked(n int64, ahead bool) (wait <-chan struct{}, onPool, nsFull bool) {
	spare, spareSlots := int64(0), 0
	if ahead {
		spare, spareSlots = maxUploadBytes, 1
	}
	if q.resident+n > q.limit-spare || q.slots+1 > cap(q.ch)-spareSlots {
		if q.room == nil {
			q.room = make(chan struct{})
		}
		return q.room, false, false
	}
	if q.pool != nil {
		if wait, nsFull := q.pool.reserve(q.share, n, spare); wait != nil {
			return wait, true, nsFull
		}
	}
	q.resident += n
	q.slots++
	return nil, false, false
}

// releaseLocked gives back n bytes and slots chunk slots. Caller holds ackMu.
func (q *sideQueue) releaseLocked(n int64, slots int) {
	if q.freed || (n == 0 && slots == 0) {
		return
	}
	q.resident -= n
	q.slots -= slots
	q.wakeLocked()
	if q.pool != nil {
		q.pool.release(q.share, n)
	}
}

// wakeLocked tells writers waiting on this direction to look again.
func (q *sideQueue) wakeLocked() {
	if q.room != nil {
		close(q.room)
		q.room = nil
	}
}

// keepLocked settles a reservation of reserved bytes for a write that turned
// out to carry used bytes: the difference goes back, and the slot too if the
// write carries nothing. Caller holds ackMu.
func (q *sideQueue) keepLocked(reserved, used int64) {
	slots := 0
	if reserved > 0 && used == 0 {
		slots = 1
	}
	q.releaseLocked(reserved-used, slots)
}

// unreserve gives back a reservation whose write was never taken.
func (q *sideQueue) unreserve(reserved int64) {
	q.ackMu.Lock()
	defer q.ackMu.Unlock()
	q.keepLocked(reserved, 0)
}

// enqueue queues b, a write reserve admitted with room for reserved bytes. It
// keeps b itself rather than a copy. Taking it is decided under ackMu, which
// close also takes, so "is it closed" and "enqueue it" cannot both look true to
// a writer racing a close: once close has returned, every later write is
// refused and its room given back.
func (q *sideQueue) enqueue(b []byte, reserved int64) error {
	q.ackMu.Lock()
	defer q.ackMu.Unlock()
	return q.enqueueLocked(b, reserved)
}

func (q *sideQueue) enqueueLocked(b []byte, reserved int64) error {
	select {
	case <-q.done:
		q.keepLocked(reserved, 0)
		return errQueueClosed
	default:
	}
	q.keepLocked(reserved, int64(len(b)))
	if len(b) > 0 {
		q.ch <- b // never waits: the reservation holds this chunk's slot
	}
	return nil
}

// commitSeq takes write number seq, which admitSeq admitted with room for
// reserved bytes, into the stream: in place if every earlier write is in, and
// otherwise into seqHeld until they are. A write taken in the meantime by a
// retry of it is acknowledged and its room given back.
func (q *sideQueue) commitSeq(seq uint64, b []byte, reserved int64) error {
	q.seqMu.Lock()
	defer q.seqMu.Unlock()
	q.ackMu.Lock()
	defer q.ackMu.Unlock()
	select {
	case <-q.done:
		q.keepLocked(reserved, 0)
		return errQueueClosed
	default:
	}
	if _, held := q.seqHeld[seq]; held || seq < q.seqNext {
		q.keepLocked(reserved, 0)
		return nil
	}
	if seq > q.seqNext {
		q.keepLocked(reserved, int64(len(b)))
		if q.seqHeld == nil {
			q.seqHeld = map[uint64][]byte{}
		}
		q.seqHeld[seq] = b
		return nil
	}
	if err := q.enqueueLocked(b, reserved); err != nil {
		return err
	}
	for {
		q.seqNext++
		next, ok := q.seqHeld[q.seqNext]
		if !ok {
			break
		}
		delete(q.seqHeld, q.seqNext)
		if len(next) > 0 {
			q.ch <- next // its slot has been held since it was admitted
		}
	}
	// A write that waited for room as out of order may be the next one now,
	// which needs less room.
	q.wakeLocked()
	return nil
}

// pushSeq is admitSeq and commitSeq for a caller that already holds the bytes,
// and it queues a copy of them. It reports false once the queue is closed or
// when seq is past the window.
func (q *sideQueue) pushSeq(seq uint64, b []byte) bool {
	n := int64(len(b))
	switch err := q.admitSeq(context.Background(), seq, n); {
	case errors.Is(err, errRepeatWrite):
		return true
	case err != nil:
		return false
	}
	return q.commitSeq(seq, bytes.Clone(b), n) == nil
}

// push enqueues a copy of b, waiting for room, or reports false once the queue
// is closed. The in-process bridge writes this way.
func (q *sideQueue) push(b []byte) bool {
	n := int64(len(b))
	if q.reserve(context.Background(), n) != nil {
		return false
	}
	return q.enqueue(bytes.Clone(b), n) == nil
}

func (q *sideQueue) close() {
	q.once.Do(func() {
		q.ackMu.Lock()
		close(q.done)
		q.ackMu.Unlock()
	})
}

// free closes the queue and gives back everything it holds, for a session that
// has left the registry: nobody can reach its bytes any more, and a poll or a
// bridge read still holding the queue must not keep them counted against the
// relay. The bytes are dropped, so the memory goes with them.
func (q *sideQueue) free() {
	q.close()
	q.seqMu.Lock()
	defer q.seqMu.Unlock()
	q.ackMu.Lock()
	defer q.ackMu.Unlock()
	if q.freed {
		return
	}
	for drained := false; !drained; {
		select {
		case <-q.ch:
		default:
			drained = true
		}
	}
	q.seqHeld = nil
	q.head = nil
	q.assigned = map[uint64][]byte{}
	q.unacked = nil
	if q.pool != nil {
		q.pool.release(q.share, q.resident)
		q.pool.leave(q.share)
	}
	q.resident, q.slots = 0, 0
	q.freed = true
	q.wakeLocked()
}

// beginTake and endTake bracket a poll's hold on the queue, so that a chunk
// which has left ch but not yet reached assigned still counts as being here.
// They are the same critical section assigned is published in, which is what
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
	return !q.inflight && len(q.assigned) == 0 && q.head == nil && len(q.ch) == 0
}

// undelivered reports whether a poll is taking bytes out of this direction
// right now, and whether it holds a chunk its reader has not acknowledged.
func (q *sideQueue) undelivered() (serving, unacked bool) {
	q.ackMu.Lock()
	defer q.ackMu.Unlock()
	return q.inflight, len(q.assigned) != 0
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
	return q.takeUpTo(ctx, ack, timeout, maxDrainBytes)
}

// takeUpTo is take with a reader-chosen bound on the chunk, at most
// maxDrainLimit.
func (q *sideQueue) takeUpTo(ctx context.Context, ack uint64, timeout time.Duration, limit int) (data []byte, seq uint64, closed, ok bool) {
	if !q.acquire(ctx) {
		return nil, 0, false, false
	}
	defer q.release()
	q.beginTake()
	defer q.endTake() // runs after the chunk is assigned, before the turn is freed

	q.ackMu.Lock()
	q.dropAcked(ack)
	for k := ack + 1; k <= q.seq; k++ {
		if b, exists := q.assigned[k]; exists {
			q.ackMu.Unlock()
			return b, k, false, true
		}
	}
	q.ackMu.Unlock()

	data, closed = q.drainUpTo(ctx, timeout, limit)
	if len(data) == 0 {
		return nil, 0, closed, true
	}
	q.ackMu.Lock()
	q.seq++
	seq = q.seq
	q.assign(seq, data)
	q.ackMu.Unlock()
	return data, seq, false, true
}

// dropAcked and assign require ackMu. The legacy unacked field remains a view
// of the first outstanding chunk for the queue's existing diagnostics. An
// acknowledged chunk is the reader's now, so its bytes leave the relay here.
func (q *sideQueue) dropAcked(ack uint64) {
	first := q.seq + 1
	q.unacked = nil
	var gone int64
	for k, b := range q.assigned {
		if k <= ack {
			delete(q.assigned, k)
			gone += int64(len(b))
		} else if k < first {
			first = k
			q.unacked = b
		}
	}
	q.releaseLocked(gone, 0)
}

func (q *sideQueue) assign(seq uint64, data []byte) {
	q.assigned[seq] = data
	if q.unacked == nil {
		q.unacked = data
	}
	close(q.changed)
	q.changed = make(chan struct{})
}

// takeWindow serves a numbered poll. A future poll waits for its predecessor
// without holding turn, so requests arriving out of order cannot deadlock the
// assignment stream. Replays bypass turn entirely.
func (q *sideQueue) takeWindow(ctx context.Context, ack, want uint64, timeout time.Duration, limit int) (data []byte, seq uint64, closed, ok bool) {
	requestCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		q.ackMu.Lock()
		q.dropAcked(ack)
		if b, exists := q.assigned[want]; exists {
			q.ackMu.Unlock()
			return b, want, false, true
		}
		if want <= q.seq {
			q.ackMu.Unlock()
			return nil, 0, false, true // a stale poll whose chunk was acknowledged
		}
		if want > q.seq+1 {
			changed := q.changed
			q.ackMu.Unlock()
			select {
			case <-changed:
				continue
			case <-ctx.Done():
				return nil, 0, false, requestCtx.Err() == nil
			}
		}
		q.ackMu.Unlock()
		if !q.acquire(ctx) {
			return nil, 0, false, requestCtx.Err() == nil
		}
		q.ackMu.Lock()
		if want != q.seq+1 {
			q.ackMu.Unlock()
			q.release()
			continue
		}
		q.inflight = true
		q.ackMu.Unlock()
		data, closed = q.drainUpTo(ctx, timeout, limit)
		q.ackMu.Lock()
		if len(data) > 0 {
			q.seq = want
			q.assign(want, data)
		}
		q.inflight = false
		q.ackMu.Unlock()
		q.release()
		if len(data) == 0 {
			return nil, 0, closed, true
		}
		return data, want, false, true
	}
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
	// Nothing will be re-sent to this reader, so the bytes leave the relay as
	// soon as they are taken.
	q.ackMu.Lock()
	q.releaseLocked(int64(len(data)), 0)
	q.ackMu.Unlock()
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
// tail served first next time, not appended whole. Chunks are bounded by
// limits.RelayHTTPUploadBytes on the /h/up path but not on the in-process
// bridge, which is why the split has to exist at all. What one direction holds
// in all is bounded by maxResidentPerDirection.
const maxDrainBytes = 2 << 20

// maxDrainLimit bounds what a reader may ask one chunk to carry (DownMaxParam).
// A reader that downloads a full chunk quickly asks for more, because each
// poll costs a round trip and each response starts on a fresh HTTP stream: on
// the relay's CDN path, from a mainland home line, 2 MiB responses moved a
// median 0.6 MB/s and 16 MiB responses 4.2 MB/s (2026-09-23). A reader on a
// slow link never grows its chunk, so issue #57's link keeps 2 MiB.
const maxDrainLimit = 16 << 20

// drain returns bytes available within timeout, coalescing queued chunks up to
// maxDrainBytes. closed is true only when the queue is closed and no more bytes
// remain. Callers hold the queue's turn.
func (q *sideQueue) drain(ctx context.Context, timeout time.Duration) (data []byte, closed bool) {
	return q.drainUpTo(ctx, timeout, maxDrainBytes)
}

// drainUpTo is drain with a bound of limit bytes instead of maxDrainBytes.
func (q *sideQueue) drainUpTo(ctx context.Context, timeout time.Duration, limit int) (data []byte, closed bool) {
	var out []byte
	// Every chunk taken out of ch frees its slot. Its bytes stay counted: the
	// caller either holds them as assigned or lets them go itself.
	taken := 0
	defer func() {
		if taken > 0 {
			q.ackMu.Lock()
			q.releaseLocked(0, taken)
			q.ackMu.Unlock()
		}
	}()
	// appendCapped takes as much of b as still fits and parks the rest in head.
	// out is grown by hand so that neither its length nor the memory behind it
	// can pass the cap, and an idle poll that never sees a byte allocates none.
	appendCapped := func(b []byte) {
		if room := limit - len(out); len(b) > room {
			q.ackMu.Lock()
			q.head = b[room:]
			q.ackMu.Unlock()
			b = b[:room]
		}
		if need := len(out) + len(b); need > cap(out) {
			grown := make([]byte, len(out), max(need, min(2*cap(out), limit)))
			copy(grown, out)
			out = grown
		}
		out = append(out, b...)
	}
	took := func(b []byte) {
		taken++
		appendCapped(b)
	}
	// fill drains what is already queued, without waiting.
	fill := func() {
		for len(out) < limit {
			select {
			case b := <-q.ch:
				took(b)
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
		took(b)
		fill()
		return out, false
	case <-time.After(timeout):
		return nil, false
	case <-ctx.Done():
		return nil, false
	case <-q.done:
		select {
		case b := <-q.ch:
			took(b)
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

// free closes the session and drops what it holds. Every path that takes a
// session out of the registry calls it; a graceful close does not, because
// the far side is still reading.
func (s *httpSession) free() {
	s.toClient.free()
	s.toAgent.free()
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
	// unackedRetention is how long a session holding an unacknowledged chunk
	// survives without a request. The relay can finish writing a chunk long
	// before its reader has it: proxies buffer what it wrote, and a reader on
	// a shaped link (20-60 KB/s over TLS on TCP) can take minutes to download
	// a large chunk and poll again. Reaping at httpSessionIdle would delete
	// bytes it is still receiving.
	unackedRetention = 10 * time.Minute
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
		// An agent from before device IDs may send no name, and is then
		// labelled by its device name; one it does send is held to the rule.
		if name := req.URL.Query().Get("name"); err == nil && name != "" && !validDeviceName(name) {
			err = errors.New("invalid device name")
		}
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
		w.Header().Set(httpconn.UpSeqCapabilityHeader, "1")
		w.Header().Set(httpconn.DownWindowCapabilityHeader, strconv.Itoa(httpconn.DownWindowSize))
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
	// Said here as well as on /h/up so the controller's first write, its TLS
	// ClientHello, need not wait for an /h/up answer to learn it.
	w.Header().Set(httpconn.UpSeqCapabilityHeader, "1")
	w.Header().Set(httpconn.DownWindowCapabilityHeader, strconv.Itoa(httpconn.DownWindowSize))
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
	// Tell the writer this relay orders sequenced writes, so it may keep
	// several in flight. A relay without this header queues /h/up in arrival
	// order and a writer must send them one at a time.
	w.Header().Set(httpconn.UpSeqCapabilityHeader, "1")
	w.Header().Set(httpconn.DownWindowCapabilityHeader, strconv.Itoa(httpconn.DownWindowSize))
	s := r.sessionForAccess(req.URL.Query().Get("session"), access, req.URL.Query().Get("role"))
	if s == nil {
		http.Error(w, "no such session", http.StatusNotFound)
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
	var seq uint64
	if seqParam := req.URL.Query().Get(httpconn.UpSeqParam); seqParam != "" {
		var err error
		seq, err = strconv.ParseUint(seqParam, 10, 64)
		if err != nil || seq == 0 {
			http.Error(w, "bad seq", http.StatusBadRequest)
			return
		}
	}
	if taken, _ := acceptUpload(w, req, dst, seq); taken {
		w.WriteHeader(http.StatusOK)
	}
}

// acceptUpload takes the body of req into dst as write number seq, or as the
// next write when seq is 0 (a writer that does not number its writes). Room for
// the body is held before a byte of it is read, so a writer that has to wait
// for room holds no memory while it waits: the bytes stay with the sender. It
// answers the request itself when the write is not taken, and reports whether
// it was, and whether it was refused because dst has closed.
func acceptUpload(w http.ResponseWriter, req *http.Request, dst *sideQueue, seq uint64) (taken, closed bool) {
	n := req.ContentLength
	if n > maxUploadBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return false, false
	}
	if n < 0 {
		n = maxUploadBytes // not known until read; the room not used goes back
	}
	var err error
	if seq == 0 {
		err = dst.reserve(req.Context(), n)
	} else {
		err = dst.admitSeq(req.Context(), seq, n)
	}
	// The server's read timeout runs from the start of the request, and the
	// wait for room may have used it up; the body gets a timeout of its own.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(limits.HTTPReadTimeout))
	switch {
	case errors.Is(err, errRepeatWrite):
		io.Copy(io.Discard, http.MaxBytesReader(w, req.Body, maxUploadBytes))
		return true, false
	case errors.Is(err, errOutOfWindow):
		// Ahead of a write that has not landed yet, most likely one being
		// retried. A 5xx is what makes the writer back off and send this one
		// again, by which time the gap has usually filled; a 4xx would end its
		// session, and holding the request would keep a connection the retry
		// may be waiting for.
		http.Error(w, "write too far ahead of the stream; retry", http.StatusServiceUnavailable)
		return false, false
	case errors.Is(err, errQueueClosed) && seq != 0:
		http.Error(w, "session closed or write out of window", http.StatusGone)
		return false, true
	case errors.Is(err, errQueueClosed):
		http.Error(w, "session closed", http.StatusGone)
		return false, true
	case err != nil:
		// The writer went away while waiting for room. Nothing was taken, so a
		// retry of this write starts afresh.
		http.Error(w, "gave up waiting for room", http.StatusServiceUnavailable)
		return false, false
	}
	body, err := readUpload(w, req, n)
	if err != nil {
		dst.unreserve(n)
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return false, false
		}
		http.Error(w, "read body", http.StatusBadRequest)
		return false, false
	}
	if seq == 0 {
		err = dst.enqueue(body, n)
	} else {
		err = dst.commitSeq(seq, body, n)
	}
	if err != nil {
		http.Error(w, "session closed", http.StatusGone)
		return false, true
	}
	return true, false
}

// readUpload reads a body of at most n bytes into a buffer no larger than it
// needs, since the queue keeps the buffer itself. A declared length is read
// into one allocation of that size; an undeclared one is trimmed after reading.
func readUpload(w http.ResponseWriter, req *http.Request, n int64) ([]byte, error) {
	r := http.MaxBytesReader(w, req.Body, n)
	if req.ContentLength >= 0 {
		body := make([]byte, req.ContentLength)
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, err
		}
		return body, nil
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if cap(body) > len(body)+len(body)/8 {
		body = bytes.Clone(body)
	}
	return body, nil
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
	w.Header().Set(httpconn.DownWindowCapabilityHeader, strconv.Itoa(httpconn.DownWindowSize))
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
		limit := maxDrainBytes
		if m, err := strconv.Atoi(req.URL.Query().Get(httpconn.DownMaxParam)); err == nil && m > 0 {
			limit = min(m, maxDrainLimit)
		}
		if req.URL.Query().Has(httpconn.DownWantParam) {
			wantParam := req.URL.Query().Get(httpconn.DownWantParam)
			want, err := strconv.ParseUint(wantParam, 10, 64)
			if err != nil || want <= ack || want-ack > httpconn.DownWindowSize {
				http.Error(w, "bad want", http.StatusBadRequest)
				return
			}
			data, seq, closed, served = src.takeWindow(req.Context(), ack, want, downPollWait, min(limit, 4<<20))
		} else {
			data, seq, closed, served = src.takeUpTo(req.Context(), ack, downPollWait, limit)
		}
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
	// Declared, not left to chunked encoding. Measured through the relay's
	// Cloudflare edge and tunnel, a 16 MiB response of unknown length slowed
	// to 0.2 MB/s part way through while the same bytes with a length moved
	// 2.2-2.9 MB/s (2026-09-23).
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	w.Write(data)
	// The reader cannot poll again before it has the chunk, so its idle time
	// starts once the chunk is written, not when the poll arrived.
	r.hmu.Lock()
	s.lastActive = time.Now()
	r.hmu.Unlock()
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
	r.hmu.Unlock()
	// A writer that knows this relay orders writes sends its last bytes with
	// the close instead of in an /h/up of their own, saving the round trip
	// that every command otherwise spends at the very end. They are queued
	// in their place before the queues shut.
	tailTaken := true
	if seqParam := req.URL.Query().Get(httpconn.UpSeqParam); seqParam != "" {
		seq, err := strconv.ParseUint(seqParam, 10, 64)
		if err != nil || seq == 0 {
			http.Error(w, "bad seq", http.StatusBadRequest)
			return
		}
		dst := s.toAgent
		if req.URL.Query().Get("role") == "agent" {
			dst = s.toClient
		}
		// Refused because the far side has already gone, the last bytes
		// cannot be delivered, but the close still is one.
		var closed bool
		if tailTaken, closed = acceptUpload(w, req, dst, seq); !tailTaken && !closed {
			return
		}
	}
	r.hmu.Lock()
	s.closedAt = time.Now()
	r.hmu.Unlock()
	// Closing the queues stops new bytes and makes the far side see EOF once
	// they run dry, but the session stays registered: with the drain cap a
	// backlog needs several more polls to come out, and deleting it here would
	// 404 them away. It leaves once both directions have been taken, or when
	// the sweeper finds nobody polling it any more.
	s.close()
	// The side that closed does not read again — not even to acknowledge the
	// last chunk it read — so what is left for it goes now. Kept, it would hold
	// the session and its bytes until the sweeper's retention ran out.
	if req.URL.Query().Get("role") == "agent" {
		s.toAgent.free()
	} else {
		s.toClient.free()
	}
	// Closing the second queue can be the last thing a finished session was
	// waiting for, and a poll that woke on the first one has already looked.
	r.releaseDrainedSession(sid, s)
	if tailTaken {
		w.WriteHeader(http.StatusOK)
	}
}

// releaseDrainedSession retires a gracefully closed session once *both*
// directions have been taken. Reaching the end of one direction says nothing
// about the other: a controller that has read everything it was sent still
// leaves the agent holding an unacknowledged chunk and a split tail, and
// retiring the session on the first EOF would 404 those away — including a
// chunk a poll has already pulled out of the queue but not yet recorded, which
// is why settled covers a take in flight and not just the fields it writes.
//
// A session torn down any other way (credential revocation, dial failure, the
// idle sweeper) is already gone from the registry and this is a no-op.
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
	if !retire {
		return
	}
	// Both directions are empty; this gives back writes still waiting in
	// seqHeld for a gap that will never fill.
	s.free()
	if s.lease != nil {
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
		idle := httpSessionIdle
		serveC, heldC := s.toClient.undelivered()
		serveA, heldA := s.toAgent.undelivered()
		if serveC || serveA {
			continue // a poll is in progress
		}
		if heldC || heldA {
			idle = unackedRetention
		}
		if !s.lastActive.IsZero() && now.Sub(s.lastActive) > idle {
			delete(r.hsess, sid)
			dead = append(dead, s)
		}
	}
	sessions := len(r.hsess)
	r.hmu.Unlock()
	for _, a := range offline {
		if !r.wsDeviceLive(a.ns + "/" + a.device) {
			r.emitDeviceEvent(a.ns, a.device, offlineEvent(a.device))
		}
	}
	for _, s := range dead {
		s.free()
		if s.lease != nil {
			s.lease.close()
		}
	}
	r.resident.report(now, sessions)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
