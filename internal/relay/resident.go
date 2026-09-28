package relay

import (
	"fmt"
	"log"
	"sync"
	"time"

	"wanctl/internal/limits"
)

// The HTTP tunnel keeps the bytes it relays in memory until the far side has
// them, so what it holds has to be bounded by something other than how fast a
// reader reads: a tenant controls both ends of its own sessions and can simply
// never read. A byte is counted from the moment a write is admitted — before
// its body is read, so a write that has to wait holds no memory while it waits —
// until it leaves: acknowledged by its reader, handed to a reader that does not
// acknowledge, or dropped with its session.
const (
	// maxResidentPerDirection bounds one direction of one session. It is sized
	// so that it never limits an honest transfer: a windowed reader holds up to
	// httpconn.DownWindowSize chunks of 4 MiB unacknowledged (16 MiB), a writer
	// keeps eight 1 MiB uploads in flight (8 MiB), and the queue needs a few MiB
	// behind the reader so that its next chunk comes out full — a short chunk
	// drops the reader back to one poll per round trip. With 16 MiB the reader's
	// window alone could fill it.
	maxResidentPerDirection = 32 << 20

	// maxResidentPerNamespace bounds the sessions one namespace dialed, taken
	// together. Without it one account that never reads could fill the
	// relay-wide budget and make every other account's writes wait for as long
	// as it keeps its sessions alive. It admits one session with both
	// directions full.
	maxResidentPerNamespace = 2 * maxResidentPerDirection

	// maxResidentTotal bounds every session on the relay together. The Go
	// collector lets the heap grow to about twice what is live, so this keeps
	// the process near half a gigabyte at worst on a 2 GB host it shares.
	maxResidentTotal = 256 << 20

	// maxUploadBytes is the largest write the tunnel accepts; room for one is
	// what an out-of-order write must leave free (see sideQueue.reserveLocked).
	maxUploadBytes = limits.RelayHTTPUploadBytes

	// residencyReportEvery is how often, at most, the sweeper logs what the
	// tunnel held, and only after it held something.
	residencyReportEvery = 10 * time.Minute
)

// residency is the account every direction of every HTTP session draws on:
// the relay's total, and each namespace's share of it.
type residency struct {
	mu      sync.Mutex
	limit   int64
	used    int64
	nsLimit int64
	ns      map[string]*nsShare
	// room is closed, when a writer is waiting on this account, each time bytes
	// are given back to it, so that the writer checks again.
	room chan struct{}

	// What the periodic report says: the most held at once, and how many
	// writes had to wait for the relay or their namespace, since the last one.
	peak, waitsTotal, waitsNS int64
	reported                  time.Time
}

// nsShare is one namespace's part of a residency. refs counts the queues that
// charge it, so that it can be dropped with the last of them.
type nsShare struct {
	name string
	used int64
	refs int
}

func newResidency() *residency {
	return &residency{limit: maxResidentTotal, nsLimit: maxResidentPerNamespace, ns: map[string]*nsShare{}, reported: time.Now()}
}

// join returns the share for namespace ns, creating it on first use, and
// counts the caller as one of its queues.
func (p *residency) join(ns string) *nsShare {
	p.mu.Lock()
	defer p.mu.Unlock()
	sh := p.ns[ns]
	if sh == nil {
		sh = &nsShare{name: ns}
		p.ns[ns] = sh
	}
	sh.refs++
	return sh
}

// leave undoes join once the queue has given back everything it held.
func (p *residency) leave(sh *nsShare) {
	if sh == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if sh.refs--; sh.refs == 0 && p.ns[sh.name] == sh {
		delete(p.ns, sh.name)
	}
}

// reserve takes n bytes from the relay and from sh (nil when the queue is not
// charged to a namespace), leaving spare bytes free at both. It returns nil on
// success, or the channel to wait on before trying again and whether it was the
// namespace's share rather than the relay's total that had no room.
func (p *residency) reserve(sh *nsShare, n, spare int64) (wait <-chan struct{}, nsFull bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	overTotal := p.used+n > p.limit-spare
	overNS := sh != nil && sh.used+n > p.nsLimit-spare
	if overTotal || overNS {
		if p.room == nil {
			p.room = make(chan struct{})
		}
		return p.room, !overTotal
	}
	p.used += n
	p.peak = max(p.peak, p.used)
	if sh != nil {
		sh.used += n
	}
	return nil, false
}

// noteWait counts a write that had to wait for room here, once per write.
func (p *residency) noteWait(nsFull bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if nsFull {
		p.waitsNS++
	} else {
		p.waitsTotal++
	}
}

// release gives n bytes back to the relay and to sh.
func (p *residency) release(sh *nsShare, n int64) {
	if n == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.used -= n
	if sh != nil {
		sh.used -= n
	}
	if p.room != nil {
		close(p.room)
		p.room = nil
	}
}

// resident reports what the relay holds now.
func (p *residency) resident() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.used
}

// report logs, at most every residencyReportEvery and only when the tunnel held
// something since the last report, the most it held at once and how often its
// budgets made a write wait. It is how the budgets above can be checked against
// real use.
func (p *residency) report(now time.Time, sessions int) {
	p.mu.Lock()
	if now.Sub(p.reported) < residencyReportEvery || (p.peak == 0 && p.waitsTotal == 0 && p.waitsNS == 0) {
		p.mu.Unlock()
		return
	}
	since := now.Sub(p.reported).Round(time.Minute)
	line := fmt.Sprintf("relay: http tunnel held at most %s in the last %s (budget %s, %s per namespace), %s now in %d sessions; writes that waited for room: %d relay-wide, %d per namespace",
		mib(p.peak), since, mib(p.limit), mib(p.nsLimit), mib(p.used), sessions, p.waitsTotal, p.waitsNS)
	p.peak, p.waitsTotal, p.waitsNS = p.used, 0, 0
	p.reported = now
	p.mu.Unlock()
	log.Print(line)
}

func mib(n int64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }
