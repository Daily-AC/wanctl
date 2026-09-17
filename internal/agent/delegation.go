package agent

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"wanctl/internal/admission"
	"wanctl/internal/eventlog"
	"wanctl/internal/protocol"
	"wanctl/internal/sessionauth"
)

type sessionAudit struct {
	grantID, credentialID, sessionID string
}

func auditSession(auth sessionauth.Open) sessionAudit {
	if auth.GrantID == "" {
		return sessionAudit{}
	}
	return sessionAudit{grantID: auth.GrantID, credentialID: auth.CredentialID, sessionID: auth.Session}
}

func (a *Agent) logSessionEvent(scope sessionAudit, e eventlog.Event) {
	if a.log == nil {
		return
	}
	e.GrantID, e.CredentialID, e.SessionID = scope.grantID, scope.credentialID, scope.sessionID
	a.log.Append(e)
}

func firstAudit(scopes []sessionAudit) sessionAudit {
	if len(scopes) != 0 {
		return scopes[0]
	}
	return sessionAudit{}
}

func rejectedRequestEvent(fp, name string, m protocol.Message, reason string) eventlog.Event {
	e := eventlog.Event{Type: "request", PeerFP: fp, PeerName: name, Detail: m.Kind, Decision: "denied: " + reason}
	switch m.Kind {
	case protocol.KindExec, protocol.KindExecAsync, protocol.KindExecPoll:
		e.Type, e.Detail, e.Cwd = "exec", m.Command, m.Cwd
	case protocol.KindFilePut:
		e.Type, e.Detail = "file", "PUT "+m.Path
	case protocol.KindFileGet:
		e.Type, e.Detail = "file", "GET "+m.Path
	case protocol.KindFileRead:
		e.Type, e.Detail = "file", "READ "+m.Path
	case protocol.KindFileEdit:
		e.Type, e.Detail = "file", "EDIT "+m.Path
	case protocol.KindFileWrite:
		e.Type, e.Detail = "file", "WRITE "+m.Path
	case protocol.KindLogs:
		e.Type, e.Detail = "logs", "read event log"
	}
	return e
}

// Check a grant through the authenticated device channel, never through a
// controller-supplied statement of its own permissions. The relay resolves its
// session credential again, so a late approval cannot revive a revoked grant.
func (a *Agent) delegationActive(ctx context.Context, auth sessionauth.Open, fp string) bool {
	if auth.GrantID == "" {
		return true
	}
	if ctx.Err() != nil || !auth.ValidFor(a.DeviceID()) || auth.ControllerFingerprint != fp {
		return false
	}
	query := url.Values{
		"device": {a.DeviceID()}, "inst": {a.inst}, "session": {auth.Session},
		"grant": {auth.GrantID}, "controller_fp": {fp},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpBase(a.opts.RelayURL)+"/agent/delegation-check?"+query.Encode(), nil)
	if err != nil {
		return false
	}
	admission.SetBearer(req, a.opts.Token)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusNoContent && ctx.Err() == nil && time.Now().Before(auth.ExpiresAt)
}

func checksPass(checks []func() bool) bool {
	for _, check := range checks {
		if check != nil && !check() {
			return false
		}
	}
	return true
}
