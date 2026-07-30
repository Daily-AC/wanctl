package portal

import (
	"encoding/json"
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
	closed  chan struct{}
	once    sync.Once
}

func newDeviceConn(conn net.Conn) *deviceConn {
	d := &deviceConn{
		conn:   conn,
		respCh: make(chan protocol.Message, 1),
		notifs: make(map[chan console.State]struct{}),
		closed: make(chan struct{}),
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
		select {
		case d.respCh <- m:
		case <-d.closed:
			return
		}
	}
}

func (d *deviceConn) rpc(req protocol.Message) (protocol.Message, error) {
	d.rpcMu.Lock()
	defer d.rpcMu.Unlock()
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
			return m, fmt.Errorf("%s", m.Reason)
		}
		return m, nil
	case <-d.closed:
		return protocol.Message{}, fmt.Errorf("device connection closed")
	case <-time.After(rpcTimeout):
		// Half-open relayed conn: the agent vanished but our leg to the relay
		// stayed up, so readLoop never errored. Tear it down so the pool evicts
		// this conn and the next request re-dials.
		d.close()
		return protocol.Message{}, fmt.Errorf("device did not respond within %s (re-dialing)", rpcTimeout)
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

func (d *deviceConn) decide(id, verdict, approver string) error {
	_, err := d.rpc(protocol.Message{Kind: protocol.KindDecide, ApprovalID: id, Verdict: verdict, Approver: approver})
	return err
}

// pairDecide trusts (verdict "y") or denies a pending controller pairing.
func (d *deviceConn) pairDecide(fp, verdict string) error {
	_, err := d.rpc(protocol.Message{Kind: protocol.KindPairDecide, FP: fp, Verdict: verdict})
	return err
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

// setLan flips the device's LAN-uplink switch. The agent replies with an
// error string in Data when the device has no LAN relay configured.
func (d *deviceConn) setLan(on bool) error {
	v := "off"
	if on {
		v = "on"
	}
	m, err := d.rpc(protocol.Message{Kind: protocol.KindLanSet, Verdict: v})
	if err != nil {
		return err
	}
	if len(m.Data) > 0 {
		var msg string
		if json.Unmarshal(m.Data, &msg) == nil && msg != "" {
			return fmt.Errorf("%s", msg)
		}
	}
	return nil
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
		d.notifMu.Unlock()
	})
}
