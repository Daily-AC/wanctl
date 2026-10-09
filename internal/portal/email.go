package portal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The contact-email door (v0.18.0). Every signed-in identity needs one
// confirmed address before it may use the portal or ask for access: without
// one the instance has no way to tell an applicant they were let in, or a
// user anything at all (owner's call, 2026-09-29). New applicants, existing
// users and administrators pass the same door; an address known from before
// is only a prefill, confirmed like any other.
//
// The door is only up where it can be passed: GitHub sign-in (identities the
// portal owns) and mail configured (links can be sent). Header-mode
// deployments and instances without SMTP behave exactly as before.
//
// The relay owns the addresses, the links and the limits
// (internal/relay/contact.go). The portal renders the pages, sends the mail,
// and turns the unconfirmed away — on its own side, so the check does not
// depend on any page's script running.

// emailRequiredBody is the exact 403 body for an API call from someone who
// has not confirmed an address, in the style of pendingInviteBody.
const emailRequiredBody = "email-required"

func (s *Server) emailGate() bool { return s.oauthEnabled() && s.mailEnabled() }

// emailNext is the door's URL for someone headed to next.
func emailNext(next string) string {
	return "/auth/email?next=" + url.QueryEscape(safeNext(next))
}

// contactStatus is the relay's answer about one identity's address.
type contactStatus struct {
	Address     string     `json:"address"`
	ConfirmedAt *time.Time `json:"confirmed_at"`
	Pending     string     `json:"pending"`
}

func (c contactStatus) confirmed() string {
	if c.ConfirmedAt == nil {
		return ""
	}
	return c.Address
}

func (s *Server) contactFor(p *principal) (contactStatus, error) {
	var out contactStatus
	resp, err := s.adminReq("GET", "/admin/contact-email",
		url.Values{"provider": {p.Provider}, "subject": {p.Subject}}, nil)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("relay admin error (status %d)", resp.StatusCode)
	}
	err = json.NewDecoder(resp.Body).Decode(&out)
	return out, err
}

// atDoor reports whether this principal must stop at the door. A relay that
// cannot answer does not send anyone to the door: the next thing that needs
// the relay fails on its own, and the door itself would fail the same way.
func (s *Server) atDoor(p *principal) bool {
	if !s.emailGate() {
		return false
	}
	c, err := s.contactFor(p)
	return err == nil && c.confirmed() == ""
}

func writeErrorJSON(w http.ResponseWriter, status int, token string, extra map[string]any) {
	body := map[string]any{"error": token}
	for k, v := range extra {
		body[k] = v
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// handleEmailPage is the door: an address field and a send button, or, once
// a link is out, where it went and what to do if it did not arrive.
func (s *Server) handleEmailPage(w http.ResponseWriter, r *http.Request) {
	if !s.emailGate() {
		http.NotFound(w, r)
		return
	}
	next := safeNext(r.URL.Query().Get("next"))
	p := s.principalFrom(r)
	if p == nil {
		http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(emailNext(next)), http.StatusSeeOther)
		return
	}
	c, err := s.contactFor(p)
	if err != nil {
		http.Error(w, "relay unreachable", http.StatusBadGateway)
		return
	}
	if c.confirmed() != "" {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	// The field starts with the address the newest link went to, else the
	// one carried over from before confirmation existed. GitHub is not asked:
	// the browser's own autofill offers the same address without an OAuth
	// permission (owner's call, 2026-09-29).
	prefill := c.Pending
	if prefill == "" {
		prefill = c.Address
	}
	state := "form"
	if c.Pending != "" {
		state = "sent"
	}
	s.render(w, "email.html", map[string]any{
		"Req": state, "Next": next, "Prefill": prefill, "SentTo": c.Pending,
		"Avatar": githubAvatarURL(p), "Initial": loginInitial(p.Login), "Login": p.Login,
	})
}

// handleEmailStatus is what the door polls while a link is out, and what the
// settings page shows.
func (s *Server) handleEmailStatus(w http.ResponseWriter, r *http.Request) {
	if !s.emailGate() {
		http.NotFound(w, r)
		return
	}
	p := s.principalFrom(r)
	if p == nil {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}
	c, err := s.contactFor(p)
	if err != nil {
		http.Error(w, "relay unreachable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"confirmed": c.confirmed(), "pending": c.Pending})
}

// handleEmailSend asks the relay for a link and mails it, synchronously: the
// person is looking at a button that says "send", and a mail server that
// refused has to be said on that screen, not in a log. The relay is told how
// it went: a delivered link supersedes the older one, a refused one is dead
// and does not hold the mailbox's interval.
func (s *Server) handleEmailSend(w http.ResponseWriter, r *http.Request) {
	if !s.emailGate() {
		http.NotFound(w, r)
		return
	}
	p := s.principalFrom(r)
	if p == nil {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}
	var in struct {
		Address string `json:"address"`
		Next    string `json:"next"`
	}
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&in)
	}
	resp, err := s.adminReq("POST", "/admin/contact-email/send", nil, map[string]string{
		"provider": p.Provider, "subject": p.Subject, "login": p.Login,
		"address": in.Address, "next": safeNext(in.Next),
	})
	if err != nil {
		writeErrorJSON(w, http.StatusBadGateway, "relay_unreachable", nil)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		var extra map[string]any
		if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			extra = map[string]any{"retry_after": secs}
		}
		token := strings.TrimSpace(string(body))
		if resp.StatusCode >= 500 || token == "" {
			token = "relay_error"
		}
		writeErrorJSON(w, resp.StatusCode, token, extra)
		return
	}
	var sent struct {
		ID        int       `json:"id"`
		Token     string    `json:"token"`
		Address   string    `json:"address"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &sent); err != nil || sent.Token == "" {
		writeErrorJSON(w, http.StatusBadGateway, "relay_error", nil)
		return
	}
	origin := s.requestOrigin(r)
	message, err := confirmMail(p.Login, sent.Address, origin, origin+"/auth/email/confirm?t="+url.QueryEscape(sent.Token))
	if err == nil {
		err = s.mail.Send(sent.Address, message)
	}
	outcome := "/admin/contact-email/sent"
	if err != nil {
		log.Printf("portal: confirmation mail for %s: %s", p.Login, mailError(err, sent.Address))
		outcome = "/admin/contact-email/failed"
	} else {
		log.Printf("portal: confirmation mail for %s sent", p.Login)
	}
	if resp, err := s.adminReq("POST", outcome, nil, map[string]int{"id": sent.ID}); err == nil {
		resp.Body.Close()
	} else {
		log.Printf("portal: confirmation link %d: relay unreachable recording %s", sent.ID, outcome)
	}
	if err != nil {
		writeErrorJSON(w, http.StatusBadGateway, "mail-failed", nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"address": sent.Address, "expires_at": sent.ExpiresAt})
}

// handleEmailCancel withdraws a pending address change from the settings
// page: the link in the mail stops working and the current address stays.
func (s *Server) handleEmailCancel(w http.ResponseWriter, r *http.Request) {
	if !s.emailGate() {
		http.NotFound(w, r)
		return
	}
	p := s.principalFrom(r)
	if p == nil {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}
	resp, err := s.adminReq("POST", "/admin/contact-email/cancel", nil, map[string]string{"provider": p.Provider, "subject": p.Subject})
	if err != nil {
		writeErrorJSON(w, http.StatusBadGateway, "relay_unreachable", nil)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		writeErrorJSON(w, http.StatusBadGateway, "relay_error", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleEmailConfirm is the page behind the link (GET) and the button on it
// (POST). The GET only describes the link, so a mail scanner that fetches
// every URL in a message confirms nothing. No session is needed: the link is
// often opened on a phone that never signed in, and the token is the proof.
func (s *Server) handleEmailConfirm(w http.ResponseWriter, r *http.Request) {
	if !s.oauthEnabled() {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodPost {
		s.confirmEmail(w, r)
		return
	}
	token := strings.TrimSpace(r.URL.Query().Get("t"))
	data := map[string]any{"Req": "unknown", "Host": s.publicHost(r)}
	if token != "" {
		resp, err := s.adminReq("POST", "/admin/contact-email/peek", nil, map[string]string{"token": token})
		if err != nil {
			http.Error(w, "relay unreachable", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		var link struct{ State, Login, Address string }
		switch resp.StatusCode {
		case http.StatusOK:
			json.NewDecoder(resp.Body).Decode(&link)
			data["Req"], data["Login"], data["Address"], data["Token"] = link.State, link.Login, link.Address, token
		case http.StatusNotFound:
		default:
			http.Error(w, "relay admin error", http.StatusBadGateway)
			return
		}
	}
	s.render(w, "email-confirm.html", data)
}

// confirmEmail spends the link. When the browser that pressed the button is
// signed in as the identity the link belongs to, it goes on to where it was
// headed; any other browser is told it is done and may close the page.
func (s *Server) confirmEmail(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&in)
	}
	if strings.TrimSpace(in.Token) == "" {
		writeErrorJSON(w, http.StatusBadRequest, "token-unknown", nil)
		return
	}
	resp, err := s.adminReq("POST", "/admin/contact-email/confirm", nil, map[string]string{"token": strings.TrimSpace(in.Token)})
	if err != nil {
		writeErrorJSON(w, http.StatusBadGateway, "relay_unreachable", nil)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		token := strings.TrimSpace(string(body))
		if resp.StatusCode >= 500 || token == "" {
			token = "relay_error"
		}
		writeErrorJSON(w, resp.StatusCode, token, nil)
		return
	}
	var link struct{ Provider, Subject, Next string }
	json.Unmarshal(body, &link)
	next := ""
	if p := s.principalFrom(r); p != nil && p.Provider == link.Provider && p.Subject == link.Subject {
		next = safeNext(link.Next)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"next": next})
}

const confirmMailSubject = "确认你的 wanctl 邮箱 · Confirm your email"

// The confirmation mail is a file under web/ for the same reasons as the
// approval mail (see mail.go).
var confirmMailPage = template.Must(template.ParseFS(assets, "web/mail-confirm.html"))

// confirmMail asks the reader to confirm one address for one GitHub account.
// It names both, because the reader may not be the person who typed the
// address, and says what to do then: nothing.
func confirmMail(login, address, origin, link string) (mailContent, error) {
	host := origin
	if u, err := url.Parse(origin); err == nil && u.Host != "" {
		host = u.Host
	}
	var page bytes.Buffer
	if err := confirmMailPage.Execute(&page, struct{ Login, Address, Host, Link string }{login, address, host, link}); err != nil {
		return mailContent{}, err
	}
	text := fmt.Sprintf("确认你的邮箱 · Confirm your email\n\n"+
		"GitHub 账号 %[1]s 在 %[3]s 填了这个邮箱 %[2]s，打开下面的链接确认：\n"+
		"The GitHub account %[1]s gave %[2]s as its email on %[3]s. Open this link to confirm:\n\n"+
		"%[4]s\n\n"+
		"链接 24 小时内有效。不是你填的，忽略这封信即可。\n"+
		"The link works for 24 hours. If this wasn't you, ignore this mail.\n", login, address, host, link)
	return mailContent{subject: confirmMailSubject, text: text, html: page.String()}, nil
}
