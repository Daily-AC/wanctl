package relay

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Contact email: one confirmed address per signed-in identity, so the
// instance can reach the people it serves (owner's call, 2026-09-29).
//
// The address belongs to the identity (provider, subject), not to the user
// row or to an access request: an applicant has no user row yet and still
// has to be told when they are let in. Only a confirmed address is ever
// mailed. Every address takes the same road, including one GitHub reports as
// verified — the person may not read that mailbox — so the portal no longer
// asks GitHub for addresses at all: it asks here for a link, mails it, and
// the address takes effect when the page behind the link is submitted.
//
// The link's GET changes nothing. Corporate mail scanners fetch every link in
// a message before a person sees it; a confirmation that happened on GET
// would be performed by the scanner.
//
// The portal is the only caller and fills provider and subject from the
// session, so these endpoints trust them the way the access-request ones do.

const (
	emailTokenTTL = 24 * time.Hour
	// emailSendsPerDay links per identity in any emailSendWindow.
	emailSendsPerDay     = 5
	emailSendWindow      = 24 * time.Hour
	emailAddressInterval = 10 * time.Minute
)

// ContactEmail is one identity's address as the portal needs to show it.
type ContactEmail struct {
	// Address is the confirmed address, or an address carried over from
	// before confirmation existed when ConfirmedAt is nil.
	Address     string     `json:"address"`
	ConfirmedAt *time.Time `json:"confirmed_at"`
	// Pending is the address the newest live link would confirm.
	Pending       string     `json:"pending,omitempty"`
	PendingSentAt *time.Time `json:"pending_sent_at,omitempty"`
}

// Confirmed is the address mail may go to, or "".
func (c ContactEmail) Confirmed() string {
	if c.ConfirmedAt == nil {
		return ""
	}
	return c.Address
}

// EmailConfirmation is one link that was sent.
type EmailConfirmation struct {
	ID        int        `json:"id"`
	Provider  string     `json:"provider"`
	Subject   string     `json:"subject"`
	Login     string     `json:"login"`
	Address   string     `json:"address"`
	Next      string     `json:"next"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	UsedAt    *time.Time `json:"used_at,omitempty"`
}

// Error tokens: exact bodies the portal matches on.
var (
	ErrEmailInvalid   = errors.New("email-invalid")
	ErrEmailUnchanged = errors.New("email-unchanged")
	// ErrEmailRateIdentity is the daily cap on links per identity.
	ErrEmailRateIdentity = errors.New("rate-identity")
	// ErrEmailRateAddress is the one-link-per-interval cap per mailbox. It
	// counts across identities: it protects the mailbox, not the sender.
	ErrEmailRateAddress = errors.New("rate-address")
	ErrTokenUnknown     = errors.New("token-unknown")
	ErrTokenUsed        = errors.New("token-used")
	// ErrTokenExpired also covers a link superseded by a newer one.
	ErrTokenExpired = errors.New("token-expired")
)

// EmailRateError is a rate-limit refusal with the moment it lifts.
type EmailRateError struct {
	Kind    error
	RetryAt time.Time
}

func (e *EmailRateError) Error() string { return e.Kind.Error() }
func (e *EmailRateError) Unwrap() error { return e.Kind }

// contactAddress validates an address someone typed. It is deliberately
// plain: one bare address, no display name, a dotted domain, and not
// GitHub's noreply relay, which accepts nothing.
func contactAddress(input string) (string, error) {
	// Reject controls before trimming so CR/LF cannot disappear into whitespace.
	for _, c := range input {
		if unicode.IsControl(c) {
			return "", ErrEmailInvalid
		}
	}
	email := strings.TrimSpace(input)
	if email == "" || len(email) > 254 {
		return "", ErrEmailInvalid
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email || addr.Name != "" {
		return "", ErrEmailInvalid
	}
	domain := email[strings.LastIndex(email, "@")+1:]
	if !strings.Contains(domain, ".") || strings.EqualFold(domain, "users.noreply.github.com") {
		return "", ErrEmailInvalid
	}
	return email, nil
}

// emailTokenHash is what is stored for a link's token. A 256-bit random token
// needs no salt or stretching; the hash only keeps a database copy from being
// a set of working links.
func emailTokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func newEmailToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("relay: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// --- HTTP ---

func (r *Relay) registerContact(mux *http.ServeMux) {
	mux.HandleFunc("/admin/contact-email", r.adminContactEmail)
	mux.HandleFunc("/admin/contact-email/send", r.adminContactEmailSend)
	mux.HandleFunc("/admin/contact-email/sent", r.adminContactEmailSent)
	mux.HandleFunc("/admin/contact-email/failed", r.adminContactEmailFailed)
	mux.HandleFunc("/admin/contact-email/peek", r.adminContactEmailPeek)
	mux.HandleFunc("/admin/contact-email/confirm", r.adminContactEmailConfirm)
}

func contactError(w http.ResponseWriter, err error) {
	var rate *EmailRateError
	switch {
	case errors.As(err, &rate):
		if secs := int(time.Until(rate.RetryAt).Seconds()) + 1; secs > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(secs))
		}
		writeErrorToken(w, http.StatusTooManyRequests, rate.Kind.Error())
	case errors.Is(err, ErrEmailInvalid):
		writeErrorToken(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrEmailUnchanged):
		writeErrorToken(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrTokenUnknown):
		writeErrorToken(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrTokenUsed), errors.Is(err, ErrTokenExpired):
		writeErrorToken(w, http.StatusGone, err.Error())
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func identityParams(provider, subject string) (string, string, bool) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	subject = strings.TrimSpace(subject)
	return provider, subject, provider != "" && subject != ""
}

// adminContactEmail answers for exactly one identity.
func (r *Relay) adminContactEmail(w http.ResponseWriter, req *http.Request) {
	if !r.requireAdminStore(w, req) || !requireMethod(w, req, http.MethodGet) {
		return
	}
	provider, subject, ok := identityParams(req.URL.Query().Get("provider"), req.URL.Query().Get("subject"))
	if !ok {
		http.Error(w, "provider and subject required", http.StatusBadRequest)
		return
	}
	out, err := r.admin.ContactEmail(provider, subject)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, out)
}

// adminContactEmailSend mints a link for an address. The response carries the
// raw token, once, for the portal to put in the mail; nothing else ever sees
// it again.
func (r *Relay) adminContactEmailSend(w http.ResponseWriter, req *http.Request) {
	if !r.requireAdminStore(w, req) || !requireMethod(w, req, http.MethodPost) {
		return
	}
	var body struct {
		Provider string `json:"provider"`
		Subject  string `json:"subject"`
		Login    string `json:"login"`
		Address  string `json:"address"`
		Next     string `json:"next"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	provider, subject, ok := identityParams(body.Provider, body.Subject)
	if !ok {
		http.Error(w, "provider and subject required", http.StatusBadRequest)
		return
	}
	address, err := contactAddress(body.Address)
	if err != nil {
		contactError(w, err)
		return
	}
	next := body.Next
	if next == "" {
		next = "/"
	}
	sent, token, err := r.admin.IssueEmailConfirmation(provider, subject, strings.TrimSpace(body.Login), address, next)
	if err != nil {
		contactError(w, err)
		return
	}
	writeJSON(w, map[string]any{"id": sent.ID, "token": token, "address": sent.Address, "expires_at": sent.ExpiresAt})
}

func linkID(w http.ResponseWriter, req *http.Request) (int, bool) {
	var body struct {
		ID int `json:"id"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.ID <= 0 {
		http.Error(w, "id required", http.StatusBadRequest)
		return 0, false
	}
	return body.ID, true
}

// adminContactEmailSent records that a link's mail was accepted by the SMTP
// server. Only now does it supersede the identity's older live link: a send
// that fails must not kill a link already sitting in the person's inbox.
func (r *Relay) adminContactEmailSent(w http.ResponseWriter, req *http.Request) {
	if !r.requireAdminStore(w, req) || !requireMethod(w, req, http.MethodPost) {
		return
	}
	id, ok := linkID(w, req)
	if !ok {
		return
	}
	if err := r.admin.MarkEmailConfirmationSent(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// adminContactEmailFailed kills a link whose mail the SMTP server refused.
func (r *Relay) adminContactEmailFailed(w http.ResponseWriter, req *http.Request) {
	if !r.requireAdminStore(w, req) || !requireMethod(w, req, http.MethodPost) {
		return
	}
	id, ok := linkID(w, req)
	if !ok {
		return
	}
	if err := r.admin.FailEmailConfirmation(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func tokenBody(w http.ResponseWriter, req *http.Request) (string, bool) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil || strings.TrimSpace(body.Token) == "" {
		http.Error(w, "token required", http.StatusBadRequest)
		return "", false
	}
	return strings.TrimSpace(body.Token), true
}

// adminContactEmailPeek describes a link without spending it: what the
// confirmation page shows before anyone presses the button. The token travels
// in a POST body so it stays out of URLs and their logs on this leg.
func (r *Relay) adminContactEmailPeek(w http.ResponseWriter, req *http.Request) {
	if !r.requireAdminStore(w, req) || !requireMethod(w, req, http.MethodPost) {
		return
	}
	token, ok := tokenBody(w, req)
	if !ok {
		return
	}
	link, err := r.admin.PeekEmailConfirmation(token)
	state := "live"
	switch {
	case errors.Is(err, ErrTokenUsed):
		state = "used"
	case errors.Is(err, ErrTokenExpired):
		state = "expired"
	case err != nil:
		contactError(w, err)
		return
	}
	writeJSON(w, map[string]any{"state": state, "login": link.Login, "address": link.Address})
}

// adminContactEmailConfirm spends a link: the address becomes the identity's
// contact address.
func (r *Relay) adminContactEmailConfirm(w http.ResponseWriter, req *http.Request) {
	if !r.requireAdminStore(w, req) || !requireMethod(w, req, http.MethodPost) {
		return
	}
	token, ok := tokenBody(w, req)
	if !ok {
		return
	}
	link, err := r.admin.ConfirmEmail(token)
	if err != nil {
		contactError(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"provider": link.Provider, "subject": link.Subject,
		"login": link.Login, "address": link.Address, "next": link.Next,
	})
}

// --- PGStore ---

// ContactEmail reads one identity's address and its newest live link in one
// round trip: the portal's pages ask on every load.
func (p *PGStore) ContactEmail(provider, subject string) (ContactEmail, error) {
	var out ContactEmail
	var address, pending sql.NullString
	var confirmedAt, sentAt sql.NullTime
	err := p.db.QueryRow(
		`SELECT c.address, c.confirmed_at, l.address, l.created_at
		   FROM (SELECT $1::text AS provider, $2::text AS subject) k
		   LEFT JOIN contact_emails c ON c.provider = k.provider AND c.subject = k.subject
		   LEFT JOIN LATERAL (
		        SELECT e.address, e.created_at FROM email_confirmations e
		         WHERE e.provider = k.provider AND e.subject = k.subject
		           AND e.used_at IS NULL AND e.expires_at > now()
		         ORDER BY e.id DESC LIMIT 1) l ON true`,
		provider, subject,
	).Scan(&address, &confirmedAt, &pending, &sentAt)
	if err != nil {
		return out, err
	}
	out.Address = address.String
	if confirmedAt.Valid {
		at := confirmedAt.Time
		out.ConfirmedAt = &at
	}
	if pending.Valid {
		out.Pending = pending.String
		at := sentAt.Time
		out.PendingSentAt = &at
	}
	return out, nil
}

const emailConfirmationColumns = `id, provider, subject, login, address, next, created_at, expires_at, used_at`

func scanEmailConfirmation(row rowScanner) (EmailConfirmation, error) {
	var out EmailConfirmation
	var usedAt sql.NullTime
	err := row.Scan(&out.ID, &out.Provider, &out.Subject, &out.Login, &out.Address, &out.Next,
		&out.CreatedAt, &out.ExpiresAt, &usedAt)
	if usedAt.Valid {
		at := usedAt.Time
		out.UsedAt = &at
	}
	return out, err
}

// IssueEmailConfirmation records a new link and returns its raw token. The
// checks and the insert share one transaction under one advisory lock, so two
// clicks on "send" cannot both slip under a limit. Every link minted counts
// against the identity's daily allowance, whether or not its mail went out:
// each is an SMTP session the instance's account pays for, and a refused
// recipient must not be a free way to make the portal dial again. The
// mailbox interval counts only mail that was not refused.
func (p *PGStore) IssueEmailConfirmation(provider, subject, login, address, next string) (EmailConfirmation, string, error) {
	tx, err := p.db.Begin()
	if err != nil {
		return EmailConfirmation{}, "", err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext('wanctl_contact_email'))`); err != nil {
		return EmailConfirmation{}, "", err
	}

	var current string
	err = tx.QueryRow(
		`SELECT address FROM contact_emails
		  WHERE provider = $1 AND subject = $2 AND confirmed_at IS NOT NULL`, provider, subject,
	).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return EmailConfirmation{}, "", err
	}
	if current != "" && strings.EqualFold(current, address) {
		return EmailConfirmation{}, "", ErrEmailUnchanged
	}

	var sends int
	var oldest sql.NullTime
	if err := tx.QueryRow(
		`SELECT count(*), min(created_at) FROM email_confirmations
		  WHERE provider = $1 AND subject = $2 AND created_at > now() - make_interval(secs => $3)`,
		provider, subject, emailSendWindow.Seconds(),
	).Scan(&sends, &oldest); err != nil {
		return EmailConfirmation{}, "", err
	}
	if sends >= emailSendsPerDay {
		return EmailConfirmation{}, "", &EmailRateError{Kind: ErrEmailRateIdentity, RetryAt: oldest.Time.Add(emailSendWindow)}
	}
	var last sql.NullTime
	if err := tx.QueryRow(
		`SELECT max(created_at) FROM email_confirmations
		  WHERE lower(address) = lower($1) AND failed_at IS NULL
		    AND created_at > now() - make_interval(secs => $2)`,
		address, emailAddressInterval.Seconds(),
	).Scan(&last); err != nil {
		return EmailConfirmation{}, "", err
	}
	if last.Valid {
		return EmailConfirmation{}, "", &EmailRateError{Kind: ErrEmailRateAddress, RetryAt: last.Time.Add(emailAddressInterval)}
	}

	token := newEmailToken()
	out, err := scanEmailConfirmation(tx.QueryRow(
		`INSERT INTO email_confirmations (token_hash, provider, subject, login, address, next, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, now() + make_interval(secs => $7))
		 RETURNING `+emailConfirmationColumns,
		emailTokenHash(token), provider, subject, login, address, next, emailTokenTTL.Seconds(),
	))
	if err != nil {
		return EmailConfirmation{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return EmailConfirmation{}, "", err
	}
	return out, token, nil
}

// MarkEmailConfirmationSent makes a delivered link the only live one for its
// identity: only the newest mail works, which is what a person who pressed
// "resend" or corrected a typo expects.
func (p *PGStore) MarkEmailConfirmationSent(id int) error {
	_, err := p.db.Exec(
		`UPDATE email_confirmations o SET expires_at = now()
		   FROM email_confirmations n
		  WHERE n.id = $1 AND o.provider = n.provider AND o.subject = n.subject
		    AND o.id <> n.id AND o.used_at IS NULL AND o.expires_at > now()`, id)
	return err
}

// FailEmailConfirmation kills a link whose mail was refused. The row stays:
// it is one of the identity's sends for the day.
func (p *PGStore) FailEmailConfirmation(id int) error {
	_, err := p.db.Exec(
		`UPDATE email_confirmations SET failed_at = now(), expires_at = LEAST(expires_at, now())
		  WHERE id = $1 AND used_at IS NULL`, id)
	return err
}

// emailConfirmationState reads a link by token and says whether it can still
// be spent. Expiry is judged by the database clock, the same one that wrote
// expires_at.
func emailConfirmationState(q accessQuerier, token string, lock bool) (EmailConfirmation, error) {
	query := `SELECT ` + emailConfirmationColumns + `, expires_at <= now()
	            FROM email_confirmations WHERE token_hash = $1`
	if lock {
		query += " FOR UPDATE"
	}
	var out EmailConfirmation
	var usedAt sql.NullTime
	var expired bool
	err := q.QueryRow(query, emailTokenHash(token)).Scan(&out.ID, &out.Provider, &out.Subject, &out.Login,
		&out.Address, &out.Next, &out.CreatedAt, &out.ExpiresAt, &usedAt, &expired)
	if errors.Is(err, sql.ErrNoRows) {
		return EmailConfirmation{}, ErrTokenUnknown
	}
	if err != nil {
		return EmailConfirmation{}, err
	}
	if usedAt.Valid {
		at := usedAt.Time
		out.UsedAt = &at
		return out, ErrTokenUsed
	}
	if expired {
		return out, ErrTokenExpired
	}
	return out, nil
}

// PeekEmailConfirmation describes a link without spending it.
func (p *PGStore) PeekEmailConfirmation(token string) (EmailConfirmation, error) {
	return emailConfirmationState(p.db, token, false)
}

// ConfirmEmail spends a link and makes its address the identity's contact
// address. Any other live link for the identity dies with it: a second mail
// still sitting in an inbox must not quietly switch the address back.
func (p *PGStore) ConfirmEmail(token string) (EmailConfirmation, error) {
	tx, err := p.db.Begin()
	if err != nil {
		return EmailConfirmation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	link, err := emailConfirmationState(tx, token, true)
	if err != nil {
		return EmailConfirmation{}, err
	}
	if _, err := tx.Exec(`UPDATE email_confirmations SET used_at = now() WHERE id = $1`, link.ID); err != nil {
		return EmailConfirmation{}, err
	}
	if _, err := tx.Exec(
		`INSERT INTO contact_emails (provider, subject, address, confirmed_at)
		 VALUES ($1, $2, $3, now())
		 ON CONFLICT (provider, subject) DO UPDATE
		   SET address = EXCLUDED.address, confirmed_at = EXCLUDED.confirmed_at`,
		link.Provider, link.Subject, link.Address,
	); err != nil {
		return EmailConfirmation{}, err
	}
	if _, err := tx.Exec(
		`UPDATE email_confirmations SET expires_at = now()
		  WHERE provider = $1 AND subject = $2 AND used_at IS NULL AND expires_at > now()`,
		link.Provider, link.Subject,
	); err != nil {
		return EmailConfirmation{}, err
	}
	if err := tx.Commit(); err != nil {
		return EmailConfirmation{}, err
	}
	return link, nil
}
