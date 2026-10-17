package agent

import (
	"context"
	"encoding/json"
	"io"

	"wanctl/internal/desktop"
	"wanctl/internal/eventlog"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
)

func (a *Agent) doOpenCU(conn io.ReadWriter, fp, peer string, m protocol.Message, audit sessionAudit, start desktop.OpenCUStart, checks ...func() bool) <-chan peerRead {
	w, err := a.getWorkspace(fp, m.Desktop.WorkspaceID)
	if err != nil {
		_ = desktop.WriteResult(conn, protocol.DesktopResult{RequestID: m.RequestID, Status: "rejected", Error: err.Error(), FailedIndex: -1}, nil)
		return nil
	}
	ctx, cancel := context.WithCancel(a.shutdown())
	defer cancel()
	pending := watchDesktopPeer(conn, cancel)
	summary := desktop.Summary("act", m.Desktop, false)
	decision := "not-executed"
	var session *uint32
	ctx = desktop.WithSessionObserver(ctx, func(id uint32) { session = &id })
	approve := func(fresh bool) bool {
		if ctx.Err() != nil || w.ctx.Err() != nil || !checksPass(checks) {
			return false
		}
		request := policy.Request{Kind: policy.KindExec, Cmd: summary, Peer: fp}
		var ok bool
		if fresh {
			// A person's stop cannot be cleared by bypass, saved rules or late grants.
			// Do not remember this decision as a rule, even if the UI offers that choice.
			a.apprMu.Lock()
			appr := a.appr
			a.apprMu.Unlock()
			decision = "denied: OpenCU stopped; fresh approval required"
			if appr != nil && appr.Ask(request).Allow {
				ok = true
				decision = "approved: OpenCU resumed by person"
			}
		} else {
			ok, decision = a.gate(request, checks...)
		}
		return ok && ctx.Err() == nil && w.ctx.Err() == nil && checksPass(checks)
	}
	res := a.desktop.DoOpenCU(ctx, w.ctx, fp+":"+w.id, m.RequestID, *m.Desktop, approve, start)
	code := 0
	var toolResult struct {
		IsError bool `json:"isError"`
	}
	if res.OpenCU != nil {
		_ = json.Unmarshal(res.OpenCU.Result, &toolResult)
	}
	if res.Status != "completed" || toolResult.IsError {
		code = 1
	}
	a.logSessionEvent(audit, eventlog.Event{Type: "exec", PeerFP: fp, PeerName: peer, Detail: summary + " [" + res.Status + "]", Decision: decision, Exit: &code, DesktopSession: session})
	a.notifyExecFinished(summary, "", peer, code)
	_ = desktop.WriteResult(conn, res, nil)
	return pending
}

func (a *Agent) PrepareDesktopUpdate() func() { return a.desktop.PauseForUpdate() }

// CloseDesktop is bounded cleanup for the production exit/re-exec path, which
// must not wait for an unrelated terminal prompt blocked in Scanln.
func (a *Agent) CloseDesktop() { a.desktop.Close() }
