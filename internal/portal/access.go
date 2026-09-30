package portal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	// EmailHint is the masked address an approval would be mailed to; the
	// relay never hands the portal the address itself on this path.
	EmailHint string `json:"email_hint,omitempty"`
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
// applicant. Only the note comes from the client; the address an approval is
// mailed to is the applicant's confirmed contact address, which the door in
// front of this form made sure exists.
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
	switch _, _, status, _ := s.resolveNamespace(p); status {
	case resolveOK, resolveNeedsEmail:
		http.Error(w, "already admitted", http.StatusConflict)
		return
	case resolveDisabled:
		http.Error(w, accountDisabledBody, http.StatusForbidden)
		return
	}
	// Checked here, not only by the page's redirect: the form is one POST
	// away from anyone with a session. A relay that cannot say fails closed.
	if s.emailGate() {
		c, err := s.contactFor(p)
		if err != nil {
			http.Error(w, "relay unreachable", http.StatusBadGateway)
			return
		}
		if c.confirmed() == "" {
			http.Error(w, emailRequiredBody, http.StatusForbidden)
			return
		}
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
	if resp.StatusCode == http.StatusOK && strings.EqualFold(strings.TrimSpace(in.Decision), "approved") && s.mailEnabled() {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			http.Error(w, "relay response incomplete", http.StatusBadGateway)
			return
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		var request struct {
			ID     int    `json:"id"`
			Login  string `json:"login"`
			Email  string `json:"email"`
			Status string `json:"status"`
		}
		if json.Unmarshal(body, &request) == nil && request.Status == "approved" && request.Email != "" {
			origin := s.requestOrigin(r)
			go func() {
				message, err := approvalMail(request.Login, origin)
				if err == nil {
					err = s.mail.Send(request.Email, message)
				}
				if err != nil {
					log.Printf("portal: approval mail request %d: %s", request.ID, mailError(err, request.Email))
				}
			}()
		}
	}
	copyResp(w, resp)
}
