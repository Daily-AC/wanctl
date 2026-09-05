package relay

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"wanctl/internal/notify"
)

// Access requests are how a stranger asks for a place on an invite-only
// deployment. Until now the only door was an administrator who already knew
// your GitHub login, so someone arriving from the product site had nowhere to
// knock.
//
// The queue is private: the relay hands the whole list only to the admin
// endpoints (portal-side gated on role), and hands an applicant nothing but
// the state of their own application. That was the owner's call on 2026-09-05,
// and it is what keeps this from becoming a public list of who wanted in and
// was turned down.
//
// Approval is not a second admission mechanism: it writes an ordinary invite
// bound to the applicant's login, the same row POST /admin/invites writes, so
// everything downstream of admission stays exactly as it was.

const (
	// accessNoteMax bounds the one free-text field an applicant controls.
	accessNoteMax = 200
	// accessRetryAfter is how long a declined account waits before asking
	// again. It is a cooling-off period, not a punishment: the point is that
	// "no" does not have to be repeated every day.
	accessRetryAfter = 7 * 24 * time.Hour

	accessPending  = "pending"
	accessApproved = "approved"
	accessDeclined = "declined"
)

// AccessRequest is one application. Subject is the immutable provider id (the
// GitHub numeric account id); Login is what an invite binds to.
type AccessRequest struct {
	ID        int        `json:"id"`
	Provider  string     `json:"provider"`
	Subject   string     `json:"subject"`
	Login     string     `json:"login"`
	Note      string     `json:"note"`
	Status    string     `json:"status"`
	CreatedAt time.Time  `json:"created_at"`
	DecidedAt *time.Time `json:"decided_at"`
	DecidedBy string     `json:"decided_by"`
}

// Error tokens, in the style of the friends endpoints: an exact body the
// portal matches on, never a sentence it has to parse.
var (
	// ErrAccessRequestOpen means one application is already waiting.
	ErrAccessRequestOpen = errors.New("request-open")
	// ErrAccessRequestCooldown means the last application was declined and
	// the waiting period has not elapsed.
	ErrAccessRequestCooldown = errors.New("request-cooldown")
	// ErrAccessRequestSettled means this account was already approved.
	ErrAccessRequestSettled = errors.New("request-approved")
)

// accessRequestGate decides whether an account may file a new application,
// given the last one it filed. It is the single rule for both the write path
// (CreateAccessRequest) and the read path (the status endpoint's can_apply),
// so the form cannot offer something the submit would refuse.
func accessRequestGate(latest AccessRequest, found bool, now time.Time) error {
	if !found {
		return nil
	}
	switch latest.Status {
	case accessPending:
		return ErrAccessRequestOpen
	case accessApproved:
		return ErrAccessRequestSettled
	case accessDeclined:
		if latest.DecidedAt != nil && now.Before(latest.DecidedAt.Add(accessRetryAfter)) {
			return ErrAccessRequestCooldown
		}
		return nil
	}
	return nil
}

// accessRetryAt is when a declined account may ask again, or the zero time if
// it may ask now.
func accessRetryAt(latest AccessRequest, found bool, now time.Time) time.Time {
	if !found || latest.Status != accessDeclined || latest.DecidedAt == nil {
		return time.Time{}
	}
	at := latest.DecidedAt.Add(accessRetryAfter)
	if now.Before(at) {
		return at
	}
	return time.Time{}
}

func normalizeAccessNote(note string) string {
	note = strings.TrimSpace(note)
	// Plain text: newlines and control characters would reach a Feishu card
	// and an HTML table, and nothing about "one sentence saying who you are"
	// needs them.
	note = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, note)
	note = strings.Join(strings.Fields(note), " ")
	if len([]rune(note)) > accessNoteMax {
		note = string([]rune(note)[:accessNoteMax])
	}
	return note
}

// --- HTTP ---

func (r *Relay) registerAccess(mux *http.ServeMux) {
	mux.HandleFunc("/admin/access-requests", r.adminAccessRequests)
	mux.HandleFunc("/admin/access-requests/status", r.adminAccessRequestStatus)
	mux.HandleFunc("/admin/access-requests/decide", r.adminAccessRequestDecide)
}

func accessError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrAccessRequestOpen), errors.Is(err, ErrAccessRequestSettled):
		writeErrorToken(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrAccessRequestCooldown):
		writeErrorToken(w, http.StatusTooManyRequests, err.Error())
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// adminAccessRequests lists the queue (GET) and files an application (POST).
// Both legs sit behind the admin secret because the portal is the only caller;
// the portal is what distinguishes an administrator reading the queue from an
// applicant filing one, and it never lets a client choose the subject.
func (r *Relay) adminAccessRequests(w http.ResponseWriter, req *http.Request) {
	if !r.requireAdminStore(w, req) {
		return
	}
	switch req.Method {
	case http.MethodGet:
		list, err := r.admin.ListAccessRequests()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"requests": list})
	case http.MethodPost:
		var body struct {
			Provider string `json:"provider"`
			Subject  string `json:"subject"`
			Login    string `json:"login"`
			Note     string `json:"note"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		body.Provider = strings.ToLower(strings.TrimSpace(body.Provider))
		body.Subject = strings.TrimSpace(body.Subject)
		body.Login = strings.ToLower(strings.TrimSpace(body.Login))
		if body.Provider == "" || body.Subject == "" || body.Login == "" {
			http.Error(w, "provider, subject and login required", http.StatusBadRequest)
			return
		}
		out, err := r.admin.CreateAccessRequest(
			body.Provider, body.Subject, body.Login, normalizeAccessNote(body.Note))
		if err != nil {
			accessError(w, err)
			return
		}
		r.emitAccessRequested(out)
		writeJSON(w, out)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// adminAccessRequestStatus answers for exactly one account. The portal fills
// provider and subject from the session, so an applicant can only ever ask
// about themselves.
func (r *Relay) adminAccessRequestStatus(w http.ResponseWriter, req *http.Request) {
	if !r.requireAdminStore(w, req) || !requireMethod(w, req, http.MethodGet) {
		return
	}
	provider := strings.ToLower(strings.TrimSpace(req.URL.Query().Get("provider")))
	subject := strings.TrimSpace(req.URL.Query().Get("subject"))
	if provider == "" || subject == "" {
		http.Error(w, "provider and subject required", http.StatusBadRequest)
		return
	}
	latest, found, err := r.admin.LatestAccessRequest(provider, subject)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	out := map[string]any{
		"status":    "none",
		"can_apply": accessRequestGate(latest, found, now) == nil,
	}
	if found {
		out["status"] = latest.Status
		out["id"] = latest.ID
		out["note"] = latest.Note
		out["created_at"] = latest.CreatedAt
		if latest.DecidedAt != nil {
			out["decided_at"] = latest.DecidedAt
		}
	}
	if at := accessRetryAt(latest, found, now); !at.IsZero() {
		out["retry_at"] = at
	}
	writeJSON(w, out)
}

// adminAccessRequestDecide approves or declines one application. Approving
// writes an invite bound to the applicant's login in the same transaction, so
// the two can never disagree about who was let in.
func (r *Relay) adminAccessRequestDecide(w http.ResponseWriter, req *http.Request) {
	if !r.requireAdminStore(w, req) || !requireMethod(w, req, http.MethodPost) {
		return
	}
	var body struct {
		ID        int    `json:"id"`
		Decision  string `json:"decision"`
		DecidedBy string `json:"decided_by"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	body.Decision = strings.ToLower(strings.TrimSpace(body.Decision))
	if body.ID <= 0 || (body.Decision != accessApproved && body.Decision != accessDeclined) {
		http.Error(w, `id and decision ("approved" or "declined") required`, http.StatusBadRequest)
		return
	}
	out, ok, err := r.admin.DecideAccessRequest(body.ID, body.Decision, strings.TrimSpace(body.DecidedBy))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		writeErrorToken(w, http.StatusNotFound, "not-found")
		return
	}
	writeJSON(w, out)
}

// emitAccessRequested tells the administrators someone is at the door, over
// the webhook path that already exists (#7). It is an account-level event:
// no device is involved, the same shape as a friend request. Every admin
// namespace with a webhook configured gets it — a deployment with no admin
// webhook simply gets nothing, which is what it asked for.
func (r *Relay) emitAccessRequested(request AccessRequest) {
	if r.notifyStore == nil || r.notifySend == nil || r.admin == nil {
		return
	}
	admins, err := r.admin.ListAdminNamespaces()
	if err != nil {
		log.Printf("relay: list admins for access.requested: %v", err)
		return
	}
	message := request.Login + " is asking for access to this instance"
	for _, ns := range admins {
		r.emitAccountEvent(ns, notify.Event{
			Event: "access.requested", Peer: request.Login,
			Message: message, Detail: request.Note,
		})
	}
}

// --- PGStore ---

const accessRequestColumns = `id, provider, subject, login, note, status,
	created_at, decided_at, COALESCE(decided_by, '')`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanAccessRequest(row rowScanner) (AccessRequest, error) {
	var out AccessRequest
	var decidedAt sql.NullTime
	err := row.Scan(&out.ID, &out.Provider, &out.Subject, &out.Login, &out.Note,
		&out.Status, &out.CreatedAt, &decidedAt, &out.DecidedBy)
	if decidedAt.Valid {
		at := decidedAt.Time
		out.DecidedAt = &at
	}
	return out, err
}

// CreateAccessRequest files one application, refusing when the account
// already has one open, was approved, or was declined too recently. The whole
// decision happens inside one transaction with the row locked, so two clicks
// on submit cannot both win.
func (p *PGStore) CreateAccessRequest(provider, subject, login, note string) (AccessRequest, error) {
	tx, err := p.db.Begin()
	if err != nil {
		return AccessRequest{}, err
	}
	defer func() { _ = tx.Rollback() }()

	latest, found, err := latestAccessRequest(tx, provider, subject, true)
	if err != nil {
		return AccessRequest{}, err
	}
	if err := accessRequestGate(latest, found, time.Now().UTC()); err != nil {
		return AccessRequest{}, err
	}
	out, err := scanAccessRequest(tx.QueryRow(
		`INSERT INTO access_requests (provider, subject, login, note)
		 VALUES ($1, $2, $3, $4)
		 RETURNING `+accessRequestColumns, provider, subject, login, note))
	if err != nil {
		return AccessRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return AccessRequest{}, err
	}
	return out, nil
}

type accessQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func latestAccessRequest(q accessQuerier, provider, subject string, lock bool) (AccessRequest, bool, error) {
	query := `SELECT ` + accessRequestColumns + `
		    FROM access_requests
		   WHERE provider = $1 AND subject = $2
		   ORDER BY id DESC LIMIT 1`
	if lock {
		query += " FOR UPDATE"
	}
	out, err := scanAccessRequest(q.QueryRow(query, provider, subject))
	if errors.Is(err, sql.ErrNoRows) {
		return AccessRequest{}, false, nil
	}
	if err != nil {
		return AccessRequest{}, false, err
	}
	return out, true, nil
}

// LatestAccessRequest returns the most recent application by one account.
func (p *PGStore) LatestAccessRequest(provider, subject string) (AccessRequest, bool, error) {
	return latestAccessRequest(p.db, provider, subject, false)
}

// ListAccessRequests returns the whole queue, waiting applications first and
// newest first within each group: the rows that need a decision are the reason
// anyone opens this list.
func (p *PGStore) ListAccessRequests() ([]AccessRequest, error) {
	rows, err := p.db.Query(`SELECT ` + accessRequestColumns + `
		  FROM access_requests
		 ORDER BY (status = 'pending') DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AccessRequest{}
	for rows.Next() {
		request, err := scanAccessRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, request)
	}
	return out, rows.Err()
}

// DecideAccessRequest records a verdict on a waiting application, and on
// approval writes the invite that admits the applicant, in one transaction.
//
// The invite insert tolerates a conflict: invites already carry a partial
// unique index over the pending logins, so a login that an administrator had
// separately pre-registered would otherwise make approval fail with a
// duplicate key — and refusing to approve someone *because they are already
// invited* is not a defensible answer.
func (p *PGStore) DecideAccessRequest(id int, status, decidedBy string) (AccessRequest, bool, error) {
	if status != accessApproved && status != accessDeclined {
		return AccessRequest{}, false, errors.New("invalid decision")
	}
	tx, err := p.db.Begin()
	if err != nil {
		return AccessRequest{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	out, err := scanAccessRequest(tx.QueryRow(
		`UPDATE access_requests
		    SET status = $2, decided_at = now(), decided_by = $3
		  WHERE id = $1 AND status = 'pending'
		  RETURNING `+accessRequestColumns, id, status, decidedBy))
	if errors.Is(err, sql.ErrNoRows) {
		return AccessRequest{}, false, nil
	}
	if err != nil {
		return AccessRequest{}, false, err
	}
	if status == accessApproved {
		if _, err := tx.Exec(
			`INSERT INTO invites (github_login, invited_by) VALUES ($1, NULLIF($2, ''))
			 ON CONFLICT (github_login) WHERE used_at IS NULL DO NOTHING`,
			out.Login, decidedBy,
		); err != nil {
			return AccessRequest{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return AccessRequest{}, false, err
	}
	return out, true, nil
}

// ListAdminNamespaces is who gets told that someone is asking to join.
func (p *PGStore) ListAdminNamespaces() ([]string, error) {
	rows, err := p.db.Query(
		`SELECT namespace FROM users WHERE role = 'admin' AND namespace <> '' ORDER BY namespace`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var ns string
		if err := rows.Scan(&ns); err != nil {
			return nil, err
		}
		out = append(out, ns)
	}
	return out, rows.Err()
}
