package portal

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"wanctl/internal/console"
	"wanctl/internal/protocol"
)

// rpcTimeout bounds a single console RPC round-trip. A relayed conn can go
// half-open: the agent process dies but our long-poll leg to the relay stays
// up, so readLoop's ReadMessage never errors and `closed` never fires. Without
// a deadline rpc() would block forever, and alive() would keep reporting the
// dead conn as usable so the pool never re-dials. On timeout we tear the conn
// down (closing `closed` -> alive()==false) so the pool evicts it and the next
// request re-dials a fresh session. A var (not const) so tests can shrink it.
var rpcTimeout = 12 * time.Second

// maxUnclaimedReplies bounds the approval_replies a connection keeps for a
// listener that has not subscribed yet. It matches the listener's buffer, so
// handing them over never blocks.
const maxUnclaimedReplies = 16

// deviceConn drives one authenticated console session to a device: it
// demultiplexes the read stream into RPC responses and unsolicited approval
// notifications, and serializes outgoing RPCs.
type deviceConn struct {
	conn    net.Conn
	rpcMu   sync.Mutex // serialize request/response round-trips
	wmu     sync.Mutex // serialize writes
	respCh  chan protocol.Message
	notifMu sync.Mutex
	notifs  map[chan console.State]struct{}
	replyCh map[chan protocol.Message]struct{} // approval_reply listeners (approval phone), guarded by notifMu
	// unclaimed holds approval_replies that arrived while nobody listened,
	// for the next replies() call. Guarded by notifMu.
	unclaimed []protocol.Message
	closed    chan struct{}
	once      sync.Once
}

func newDeviceConn(conn net.Conn) *deviceConn {
	d := &deviceConn{
		conn:    conn,
		respCh:  make(chan protocol.Message, 1),
		notifs:  make(map[chan console.State]struct{}),
		replyCh: make(map[chan protocol.Message]struct{}),
		closed:  make(chan struct{}),
	}
	go d.readLoop()
	return d
}

func (d *deviceConn) readLoop() {
	defer d.close()
	for {
		m, err := protocol.ReadMessage(d.conn)
		if err != nil {
			return
		}
		if m.Kind == protocol.KindApprovalNotif {
			var st console.State
			if json.Unmarshal(m.Data, &st) == nil {
				d.notifMu.Lock()
				for ch := range d.notifs {
					select {
					case ch <- st:
					default:
					}
				}
				d.notifMu.Unlock()
			}
			continue
		}
		if m.Kind == protocol.KindApprovalReply {
			// Unsolicited, like the notification above: the owner decided on
			// the approval phone. It must never land in respCh, where it would
			// be taken for the answer to whatever RPC is in flight.
			d.notifMu.Lock()
			if len(d.replyCh) == 0 {
				// The phone resends what it has not seen settled as soon as a
				// session opens, which is before the portal subscribes. Keep
				// it for the first listener instead of dropping the only copy.
				if len(d.unclaimed) == maxUnclaimedReplies {
					d.unclaimed = d.unclaimed[1:]
				}
				d.unclaimed = append(d.unclaimed, m)
			}
			for ch := range d.replyCh {
				select {
				case ch <- m:
				default:
				}
			}
			d.notifMu.Unlock()
			continue
		}
		select {
		case d.respCh <- m:
		case <-d.closed:
			return
		}
	}
}

func (d *deviceConn) rpc(req protocol.Message) (protocol.Message, error) {
	return d.rpcWithin(req, rpcTimeout)
}

func (d *deviceConn) rpcWithin(req protocol.Message, timeout time.Duration) (protocol.Message, error) {
	d.rpcMu.Lock()
	defer d.rpcMu.Unlock()
	return d.rpcLocked(req, timeout)
}

// deviceRefusedError is a device's KindError answer: the device is there and
// said no, as opposed to a connection that failed or went quiet.
type deviceRefusedError struct{ reason string }

func (e *deviceRefusedError) Error() string { return e.reason }

// errDeviceBusy means another RPC to this device is in flight. Only callers
// that would rather skip a device than queue behind it see it.
var errDeviceBusy = errors.New("device busy")

// tryRPCWithin is rpcWithin for a caller that must not wait behind another
// RPC: a slow device holds rpcMu for up to the whole timeout, and callers that
// queue there pile up one goroutine per request.
func (d *deviceConn) tryRPCWithin(req protocol.Message, timeout time.Duration) (protocol.Message, error) {
	if !d.rpcMu.TryLock() {
		return protocol.Message{}, errDeviceBusy
	}
	defer d.rpcMu.Unlock()
	return d.rpcLocked(req, timeout)
}

// rpcLocked sends req and waits for its reply. The caller holds rpcMu.
func (d *deviceConn) rpcLocked(req protocol.Message, timeout time.Duration) (protocol.Message, error) {
	d.wmu.Lock()
	err := protocol.WriteMessage(d.conn, req)
	d.wmu.Unlock()
	if err != nil {
		// Conn is broken. Drain any late response so it cannot contaminate a
		// future RPC's read on the shared respCh (cap 1, single in-flight).
		select {
		case <-d.respCh:
		default:
		}
		return protocol.Message{}, err
	}
	select {
	case m := <-d.respCh:
		if m.Kind == protocol.KindError {
			reason := m.Reason
			if reason == "" {
				// The console RPC handler puts its reason in Data as a JSON
				// string ("unknown RPC kind" from an older agent).
				_ = json.Unmarshal(m.Data, &reason)
			}
			return m, &deviceRefusedError{reason: reason}
		}
		return m, nil
	case <-d.closed:
		return protocol.Message{}, fmt.Errorf("device connection closed")
	case <-time.After(timeout):
		// Half-open relayed conn: the agent vanished but our leg to the relay
		// stayed up, so readLoop never errored. Tear it down so the pool evicts
		// this conn and the next request re-dials.
		d.close()
		return protocol.Message{}, fmt.Errorf("device did not respond within %s (re-dialing)", timeout)
	}
}

func (d *deviceConn) state() (console.State, error) {
	m, err := d.rpc(protocol.Message{Kind: protocol.KindConsoleState})
	if err != nil {
		return console.State{}, err
	}
	var st console.State
	return st, json.Unmarshal(m.Data, &st)
}

// stateIfIdle is state for the aggregate view: it skips a device that is busy
// answering someone else and gives up after timeout.
func (d *deviceConn) stateIfIdle(timeout time.Duration) (console.State, error) {
	m, err := d.tryRPCWithin(protocol.Message{Kind: protocol.KindConsoleState}, timeout)
	if err != nil {
		return console.State{}, err
	}
	var st console.State
	return st, json.Unmarshal(m.Data, &st)
}

func (d *deviceConn) decide(id, verdict, approver string) error {
	_, err := d.rpc(protocol.Message{Kind: protocol.KindDecide, ApprovalID: id, Verdict: verdict, Approver: approver})
	return err
}

// decideFound is decide for a caller that must know whether the request was
// still there: the device answers "not-found" once the wait has run out, and a
// phone approval arriving then becomes a late grant instead (ADR 0015).
func (d *deviceConn) decideFound(id, verdict, approver string) (bool, error) {
	m, err := d.rpc(protocol.Message{Kind: protocol.KindDecide, ApprovalID: id, Verdict: verdict, Approver: approver})
	if err != nil {
		return false, err
	}
	return m.Verdict == "ok", nil
}

// grantOnce installs a late approval on the device: one request of this kind,
// from this controller, with this label (command label or path), allowed once
// within ttl.
func (d *deviceConn) grantOnce(kind, pattern, fp string, ttl time.Duration, approver string) error {
	_, err := d.rpc(protocol.Message{Kind: protocol.KindGrantOnce, RuleKind: kind, Pattern: pattern, FP: fp,
		TimeoutSec: int(ttl / time.Second), Approver: approver})
	return err
}

// approvalPush shows or updates a card on the approval phone. The timeout is
// short on purpose: a phone that cannot acknowledge within it is treated as
// offline, and the request it was meant for is refused rather than left
// hanging for the full wait.
func (d *deviceConn) approvalPush(card protocol.ApprovalCard, timeout time.Duration) error {
	data, err := json.Marshal(card)
	if err != nil {
		return err
	}
	_, err = d.rpcWithin(protocol.Message{Kind: protocol.KindApprovalPush, Data: data}, timeout)
	return err
}

// replies returns a channel carrying every approval_reply the device sends from
// now on, plus an idempotent cancel. Like subscribe, the channel is closed when
// the connection dies, so the listener knows to treat the phone as gone.
func (d *deviceConn) replies() (<-chan protocol.Message, func()) {
	ch := make(chan protocol.Message, maxUnclaimedReplies)
	d.notifMu.Lock()
	select {
	case <-d.closed:
		close(ch)
		d.notifMu.Unlock()
		return ch, func() {}
	default:
		d.replyCh[ch] = struct{}{}
		for _, m := range d.unclaimed {
			ch <- m // cap(ch) >= maxUnclaimedReplies
		}
		d.unclaimed = nil
	}
	d.notifMu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			d.notifMu.Lock()
			if _, ok := d.replyCh[ch]; ok {
				delete(d.replyCh, ch)
				close(ch)
			}
			d.notifMu.Unlock()
		})
	}
}

// errPairingGone means the device no longer holds that pending pairing: it
// expired (pairTTL) or somebody already answered it. It is not a transport
// failure, and the two want different words in front of a person.
var errPairingGone = errors.New("pairing_gone")

// pairDecide trusts (verdict "y") or denies a pending controller pairing.
//
// The agent reports a refusal by echoing the same kind back with the reason in
// Data rather than as KindError, so rpc sees a well-formed reply and returns no
// error. Reading Data is what turns "the device dropped that request" from a
// silent 200 into something the browser can say out loud (issue #79).
func (d *deviceConn) pairDecide(fp, verdict string) error {
	m, err := d.rpc(protocol.Message{Kind: protocol.KindPairDecide, FP: fp, Verdict: verdict})
	if err != nil {
		return err
	}
	var reason string
	if len(m.Data) > 0 && json.Unmarshal(m.Data, &reason) == nil && reason != "" {
		return errPairingGone
	}
	return nil
}

// untrust drops a trusted controller from the device by fingerprint.
func (d *deviceConn) untrust(fp string) error {
	_, err := d.rpc(protocol.Message{Kind: protocol.KindTrustRevoke, FP: fp})
	return err
}

func (d *deviceConn) addRule(kind, pattern, dir, scope string) error {
	_, err := d.rpc(protocol.Message{Kind: protocol.KindRuleAdd, RuleKind: kind, Pattern: pattern, Dir: dir, Scope: scope})
	return err
}

func (d *deviceConn) removeRule(i int) error {
	_, err := d.rpc(protocol.Message{Kind: protocol.KindRuleRm, Index: i})
	return err
}

// setApprovalTimeout raises (or, with 0, restores the default of) how long the
// device blocks waiting for an approval decision. It returns the seconds the
// device actually applied, which may differ: the device clamps the request into
// its own accepted range rather than rejecting it.
func (d *deviceConn) setApprovalTimeout(sec int) (int, error) {
	m, err := d.rpc(protocol.Message{Kind: protocol.KindTimeoutSet, TimeoutSec: sec})
	if err != nil {
		return 0, err
	}
	return m.TimeoutSec, nil
}

func (d *deviceConn) setMode(mode string) error {
	_, err := d.rpc(protocol.Message{Kind: protocol.KindModeSet, ConsoleMode: mode})
	return err
}

// logs requests the device's event-log lines over the console session. The
// returned RawMessage is a JSON array of eventlog.Event, forwarded verbatim to
// the portal SPA.
func (d *deviceConn) logs(logType, grep, since string, limit int) (json.RawMessage, error) {
	m, err := d.rpc(protocol.Message{Kind: protocol.KindLogs, LogType: logType, Grep: grep, Since: since, Limit: limit})
	if err != nil {
		return nil, err
	}
	if len(m.Data) == 0 {
		return json.RawMessage("[]"), nil
	}
	return m.Data, nil
}

// subscribe returns a channel carrying every approval notification pushed by the
// device from now on, plus an idempotent cancel func. Each subscriber gets its
// own buffer so a stalled consumer only ever drops its own events (readLoop
// delivers non-blocking) — a long-poll browser request and a resident background
// watcher must not steal notifications from each other.
//
// The channel is closed when the subscription is cancelled AND when the
// connection dies, so a resident consumer can tell "no events yet" from "this
// conn is gone, re-dial" without polling alive().
func (d *deviceConn) subscribe() (<-chan console.State, func()) {
	ch := make(chan console.State, 8)

	d.notifMu.Lock()
	select {
	case <-d.closed:
		close(ch)
		d.notifMu.Unlock()
		return ch, func() {}
	default:
		d.notifs[ch] = struct{}{}
	}
	d.notifMu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			d.notifMu.Lock()
			// close() may have already closed and unregistered this channel;
			// only the holder of the map entry closes, so a double close is
			// impossible in either order.
			if _, ok := d.notifs[ch]; ok {
				delete(d.notifs, ch)
				close(ch)
			}
			d.notifMu.Unlock()
		})
	}
}

// alive reports whether the connection is still usable (its read loop has not
// torn down). A device restart / network drop closes the conn from the read
// side; the pool must not hand back a dead conn.
func (d *deviceConn) alive() bool {
	select {
	case <-d.closed:
		return false
	default:
		return true
	}
}

func (d *deviceConn) close() {
	d.once.Do(func() {
		close(d.closed)
		d.conn.Close()
		// Wake every live subscriber. Without this a resident consumer parks on
		// a channel nothing will ever write to again and never learns it must
		// re-dial; a request-scoped consumer only got away with it because its
		// own context or poll deadline fired. Taking notifMu here also closes
		// the race where subscribe() saw an open conn microseconds before this.
		d.notifMu.Lock()
		for ch := range d.notifs {
			delete(d.notifs, ch)
			close(ch)
		}
		for ch := range d.replyCh {
			delete(d.replyCh, ch)
			close(ch)
		}
		d.notifMu.Unlock()
	})
}

// Pairing has its own 30-second device deadline. Leave room for relayed delivery.
func (d *deviceConn) pairADB(port int, code string) error {
	reply, err := d.rpcWithin(protocol.Message{Kind: protocol.KindADBPair, PairPort: port, PairCode: code}, 40*time.Second)
	if err != nil {
		return err
	}
	if reply.Kind != protocol.KindADBPair {
		return fmt.Errorf("device does not support ADB pairing; update its Android app")
	}
	return nil
}
