package agent

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/netip"
	"time"

	"github.com/quic-go/quic-go"
	"wanctl/internal/direct"
	"wanctl/internal/eventlog"
	"wanctl/internal/protocol"
	"wanctl/internal/server"
)

const directMaxActive int32 = 4

func (a *Agent) tryDirectSlot() bool {
	for {
		active := a.directActive.Load()
		if active >= directMaxActive {
			return false
		}
		if a.directActive.CompareAndSwap(active, active+1) {
			return true
		}
	}
}

func (a *Agent) directFilePut(conn *tls.Conn, m protocol.Message, root string, audit sessionAudit) (<-chan peerRead, bool) {
	upload, err := server.PrepareFilePut(m, root, protocol.MaxFileSize)
	if err != nil {
		protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: err.Error()})
		return nil, false
	}
	return a.directFile(conn, m.Size, protocol.Message{Kind: protocol.KindOK}, audit, upload.Close,
		func(rw io.ReadWriter, progress func(int)) { upload.TransferWithProgress(rw, m, progress) })
}

func (a *Agent) directFileGet(conn *tls.Conn, m protocol.Message, root string, audit sessionAudit) (<-chan peerRead, bool) {
	download, err := server.PrepareFileGet(m, root)
	if err != nil {
		protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: err.Error()})
		return nil, false
	}
	meta := download.Meta()
	return a.directFile(conn, meta.Size, meta, audit, download.Close,
		func(rw io.ReadWriter, progress func(int)) { download.TransferWithProgress(rw, progress) })
}

func (a *Agent) directEvent(audit sessionAudit, detail string) {
	a.logSessionEvent(audit, eventlog.Event{Type: "direct", Detail: detail, Decision: "accepted"})
}

func (a *Agent) directFile(conn *tls.Conn, size int64, first protocol.Message, audit sessionAudit,
	closeFile func(), transfer func(io.ReadWriter, func(int))) (<-chan peerRead, bool) {
	owned := true
	defer func() {
		if owned {
			closeFile()
		}
	}()
	relay := func() (<-chan peerRead, bool) {
		if err := protocol.WriteMessage(conn, first); err == nil {
			transfer(conn, nil)
		}
		return nil, false
	}
	if size < a.directSettings.MinBytes || !a.tryDirectSlot() {
		return relay()
	}
	slotOwned := true
	defer func() {
		if slotOwned {
			a.directActive.Add(-1)
		}
	}()
	releaseSlot := func() {
		if slotOwned {
			a.directActive.Add(-1)
			slotOwned = false
		}
	}
	ep, err := direct.Open(a.shutdown(), a.directSettings)
	if err != nil {
		releaseSlot()
		a.directEvent(audit, "fallback:socket")
		return relay()
	}
	epOwned := true
	defer func() {
		if epOwned {
			ep.Close()
		}
	}()
	if len(ep.Info().Candidates) == 0 {
		ep.Close()
		epOwned = false
		releaseSlot()
		a.directEvent(audit, "fallback:no candidates")
		return relay()
	}
	first.Direct = ep.Info()
	offerSent := time.Now()
	if err := protocol.WriteMessage(conn, first); err != nil {
		a.directEvent(audit, "fallback:offer write failed")
		return nil, true
	}
	holdCtx, stopHold := context.WithDeadline(a.shutdown(), offerSent.Add(a.directSettings.Hold))
	defer stopHold()
	pending := readDirectRelay(conn)
	var accepts <-chan direct.AcceptResult
	attachments := make(chan directAttachment, 8)
	offered := false
	for {
		select {
		case read := <-pending:
			pending = nil
			if read.err != nil {
				a.directEvent(audit, "fallback:relay closed during hold")
				return nil, true
			}
			switch read.msg.Kind {
			case protocol.KindDirectOffer:
				if offered || read.msg.Direct == nil {
					a.directEvent(audit, "fallback:invalid offer")
					protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: "invalid direct_offer"})
					return nil, true
				}
				offered = true
				accepts, err = ep.Listen(holdCtx, read.msg.Direct)
				if err != nil {
					accepts = nil
				}
				pending = readDirectRelay(conn)
			case protocol.KindDirectFallback:
				ep.Close()
				epOwned = false
				releaseSlot()
				a.directEvent(audit, "fallback:controller")
				transfer(conn, nil)
				return nil, false
			default:
				a.directEvent(audit, "fallback:unexpected request")
				protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: "unexpected request during direct hold: " + read.msg.Kind})
				return nil, true
			}
		case result := <-accepts:
			if result.Conn == nil {
				continue
			}
			go waitDirectAttach(holdCtx, result.Conn, attachments)
		case result := <-attachments:
			if result.err != nil {
				result.conn.CloseWithError(1, "attach failed")
				continue
			}
			if !a.enter() {
				a.directEvent(audit, "fallback:agent shutdown")
				protocol.WriteMessage(result.stream, protocol.Message{Kind: protocol.KindError, Reason: "agent shutting down"})
				result.conn.CloseWithError(1, "shutdown")
				return pending, true
			}
			if err := protocol.WriteMessage(result.stream, protocol.Message{Kind: protocol.KindOK}); err != nil {
				a.directEvent(audit, "fallback:attach acknowledgement failed")
				a.leave()
				result.conn.CloseWithError(1, "ack failed")
				return pending, true
			}
			ep.CloseListenersExcept(result.conn)
			a.directEvent(audit, "established:"+candidateType(result.conn.RemoteAddr().String()))
			owned, epOwned, slotOwned = false, false, false
			go a.runDirectTransfer(ep, result.conn, result.stream, closeFile, transfer)
			return pending, false
		case <-holdCtx.Done():
			protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: "direct hold timed out"})
			a.directEvent(audit, "fallback:hold timed out")
			return pending, true
		}
	}
}

func candidateType(remote string) string {
	ap, err := netip.ParseAddrPort(remote)
	if err != nil {
		return "host"
	}
	if ap.Addr().Is6() {
		return "ipv6"
	}
	if ap.Addr().IsPrivate() || ap.Addr().IsLoopback() {
		return "host"
	}
	return "reflexive"
}

func readDirectRelay(conn io.Reader) <-chan peerRead {
	ch := make(chan peerRead, 1)
	go func() { msg, err := protocol.ReadMessage(conn); ch <- peerRead{msg: msg, err: err} }()
	return ch
}

type directAttachment struct {
	conn   *quic.Conn
	stream *quic.Stream
	err    error
}

func waitDirectAttach(ctx context.Context, conn *quic.Conn, out chan<- directAttachment) {
	stream, err := conn.AcceptStream(ctx)
	if err == nil {
		if deadline, ok := ctx.Deadline(); ok {
			_ = stream.SetReadDeadline(deadline)
		}
		var msg protocol.Message
		msg, err = protocol.ReadMessage(stream)
		if err == nil && msg.Kind != protocol.KindDirectAttach {
			err = fmt.Errorf("expected direct_attach, got %s", msg.Kind)
		}
		_ = stream.SetReadDeadline(time.Time{})
	}
	select {
	case out <- directAttachment{conn: conn, stream: stream, err: err}:
	case <-ctx.Done():
		conn.CloseWithError(1, "hold ended")
	}
}

func (a *Agent) runDirectTransfer(ep *direct.Endpoint, conn *quic.Conn, stream *quic.Stream,
	closeFile func(), transfer func(io.ReadWriter, func(int))) {
	defer a.leave()
	defer ep.Close()
	defer conn.CloseWithError(0, "")
	released := false
	defer func() {
		if !released {
			closeFile()
			a.directActive.Add(-1)
		}
	}()
	watchdog := time.AfterFunc(a.directSettings.NoProgress, func() { conn.CloseWithError(1, "no file progress") })
	defer watchdog.Stop()
	stop := context.AfterFunc(a.shutdown(), func() { conn.CloseWithError(1, "agent shutdown") })
	defer stop()
	transfer(stream, func(n int) {
		if n > 0 {
			watchdog.Reset(a.directSettings.NoProgress)
		}
	})
	_ = stream.Close()
	closeFile()
	a.directActive.Add(-1)
	released = true
	<-conn.Context().Done()
}
