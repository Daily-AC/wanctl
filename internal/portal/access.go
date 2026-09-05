package portal

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Access requests: the portal half of "let a stranger ask to be let in".
//
// The applicant is necessarily signed in — GitHub OAuth is how the portal
// learns which login an invite would be issued against — but has no namespace
// yet, so none of the /api handlers apply: requireNS would turn them away with
// the very "pending-invite" that brought them here. These handlers therefore
// authenticate on the principal alone, and never take the subject from the
// client: the relay is told who is applying by the session, not by the body.
//
// The queue is private (owner's call, 2026-09-05). Reading it or deciding on
// it is admin-only, enforced here the same way invites are, so a plain user
// cannot see who else asked or what they wrote.

// accessNoteMax is the note length the form advertises. The relay enforces
// the same number (relay.accessNoteMax) and truncates rather than refusing, so
// the two drifting apart costs a trimmed sentence, never a lost application.
const accessNoteMax = 200

// accessStatus is what the pending page and the applicant's own poll get back:
// their own application and nothing else.
type accessStatus struct {
	Status   string     `json:"status"`
	CanApply bool       `json:"can_apply"`
	RetryAt  *time.Time `json:"retry_at,omitempty"`
}

// accessStatusFor asks the relay about one principal's own application.
func (s *Server) accessStatusFor(p *principal) (accessStatus, error) {
	out := accessStatus{Status: "none", CanApply: true}
	resp, err := s.adminReq("GET", "/admin/access-requests/status",
		url.Values{"provider": {p.Provider}, "subject": {p.Subject}}, nil)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("relay admin error (status %d)", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, err
	}
	return out, nil
}

// handleAccessRequest files an application on behalf of the signed-in
// applicant. Only the note comes from the client.
func (s *Server) handleAccessRequest(w http.ResponseWriter, r *http.Request) {
	if !s.oauthEnabled() {
		http.NotFound(w, r)
		return
	}
	p := s.principalFrom(r)
	if p == nil {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}
	// Someone who already has a namespace has nothing to apply for, and an
	// application from them would sit in the queue forever.
	if _, _, status, _ := s.resolveNamespace(p, ""); status == resolveOK {
		http.Error(w, "already admitted", http.StatusConflict)
		return
	}
	var in struct {
		Note string `json:"note"`
	}
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&in)
	}
	resp, err := s.adminReq("POST", "/admin/access-requests", nil, map[string]string{
		"provider": p.Provider,
		"subject":  p.Subject,
		"login":    p.Login,
		"note":     strings.TrimSpace(in.Note),
	})
	if err != nil {
		http.Error(w, "relay unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyResp(w, resp)
}

// handleAccessRequests hands the queue to an administrator.
func (s *Server) handleAccessRequests(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	resp, err := s.adminReq("GET", "/admin/access-requests", nil, nil)
	if err != nil {
		http.Error(w, "relay unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyResp(w, resp)
}

// handleAccessDecide approves or declines one application. Approving issues
// the ordinary invite bound to that login, relay-side and in one transaction.
func (s *Server) handleAccessDecide(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	var in struct {
		ID       int    `json:"id"`
		Decision string `json:"decision"`
	}
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&in)
	}
	resp, err := s.adminReq("POST", "/admin/access-requests/decide", nil, map[string]any{
		"id": in.ID, "decision": strings.TrimSpace(in.Decision), "decided_by": ns,
	})
	if err != nil {
		http.Error(w, "relay unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyResp(w, resp)
}
