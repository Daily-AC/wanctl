package agent

import (
	"context"
	"crypto/tls"
	"io"

	"wanctl/internal/desktop"
	"wanctl/internal/eventlog"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
)

func (a *Agent) doDesktop(conn *tls.Conn, fp, peerName string, m protocol.Message, audit sessionAudit, checks ...func() bool) <-chan peerRead {
	return a.doDesktopUsing(conn, fp, peerName, m, audit, desktop.Supported, desktop.RunHelper, checks...)
}

// The seam keeps protocol, approval and disconnect tests independent of a
// real desktop; only the production wrapper chooses the native helper.
func (a *Agent) doDesktopUsing(conn io.ReadWriter, fp, peerName string, m protocol.Message, audit sessionAudit, supported bool, run desktop.Runner, checks ...func() bool) <-chan peerRead {
	// A new non-Windows agent preserves this connection for the old screenshot
	// verb; an old agent's unknown-request response closes it instead.
	if !supported {
		_ = protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: protocol.DesktopWindowsOnly})
		return nil
	}
	if err := desktop.Validate(m.Action, m.Desktop); err != nil {
		_ = desktop.WriteResult(conn, protocol.DesktopResult{RequestID: m.RequestID, Status: "rejected", Error: err.Error(), FailedIndex: -1}, nil)
		return nil
	}
	ctx, cancel := context.WithCancel(a.shutdown())
	defer cancel()
	// Observe cancellation while approval is pending too. This stream accepts
	// one desktop call at a time; pipelining another request cancels the batch
	// before handing that request back to the normal request loop.
	pending := watchDesktopPeer(conn, cancel)
	command := desktop.Summary(m.Action, m.Desktop, false)
	detail := desktop.Summary(m.Action, m.Desktop, true)
	ok, decision := a.gate(policy.Request{Kind: policy.KindExec, Cmd: command, Peer: fp}, checks...)
	if ok && (!checksPass(checks) || ctx.Err() != nil) {
		ok, decision = false, "delegation inactive"
	}
	if !ok {
		a.logSessionEvent(audit, eventlog.Event{Type: "exec", PeerFP: fp, PeerName: peerName, Detail: detail, Decision: decision})
		_ = protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindReject, Reason: "command denied by device policy: " + command})
		return pending
	}
	label := peerName
	if a.known != nil {
		if peer, ok := a.known.Get(fp); ok && peer.Label != "" {
			label = peer.Label + " (" + peer.Name + ")"
		}
	}
	label = fp[:min(len(fp), 20)] + " · " + label
	res, data := a.desktop.Do(ctx, fp, label, m.Action, m.RequestID, m.Desktop, run)
	code := 0
	if res.Status != "completed" {
		code = 1
	}
	// No OS window titles, typed text, launch arguments, or helper output enter
	// these surfaces. Status alone is sufficient to audit interrupted batches.
	a.logSessionEvent(audit, eventlog.Event{Type: "exec", PeerFP: fp, PeerName: peerName, Detail: detail + " [" + res.Status + "]", Decision: decision, Exit: &code})
	a.notifyExecFinished(detail, "", peerName, code)
	_ = desktop.WriteResult(conn, res, data)
	return pending
}

func watchDesktopPeer(conn io.Reader, cancel context.CancelFunc) <-chan peerRead {
	ch := make(chan peerRead, 1)
	go func() { m, err := protocol.ReadMessage(conn); cancel(); ch <- peerRead{msg: m, err: err} }()
	return ch
}
