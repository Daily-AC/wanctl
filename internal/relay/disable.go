package relay

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"wanctl/internal/admission"
)

// Disabling an account (security review S-05) is one flag on the user row,
// checked where credentials are resolved instead of revoking them one by one.
// A disabled namespace's tokens, OAuth grants and WebFetch grants stop
// resolving (PGStore.Resolve, ResolveAccess, ExtendRelayTokenHash), and so do
// its portal session and a fresh GitHub login (ResolveIdentity, which the
// portal asks on every request). Clearing the flag brings all of it back
// unchanged: devices reconnect with the tokens they already hold.

// ErrAccountDisabled is ResolveIdentity's answer for a disabled account.
var ErrAccountDisabled = errors.New(accountDisabledBody)

// accountDisabledBody is the exact 403 body /admin/resolve-user returns for a
// disabled account. Contract with internal/portal.
const accountDisabledBody = "account-disabled"

// AccountDisabler reads and sets the flag. PGStore implements it.
type AccountDisabler interface {
	AccountDisabled(namespace string) (at *time.Time, found bool, err error)
	SetAccountDisabled(namespace string, disabled bool) (at *time.Time, found bool, err error)
}

// namespaceEnabled is the predicate every credential lookup ANDs in. col names
// the namespace column of the row being resolved. A namespace with no user row
// (the portal's own, a static token) is never disabled.
func namespaceEnabled(col string) string {
	return `NOT EXISTS (SELECT 1 FROM users du WHERE du.namespace = ` + col + ` AND du.disabled_at IS NOT NULL)`
}

// TokenAccountDisabled reports that token is a live token of a disabled
// account, which Resolve refuses like any other.
func (p *PGStore) TokenAccountDisabled(token string) bool {
	var disabled bool
	err := p.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM tokens t JOIN users u ON u.namespace = t.namespace
		WHERE t.hash = $1 AND t.revoked_at IS NULL AND t.kind <> 'delegated'
		  AND (t.expires_at IS NULL OR t.expires_at > now()) AND u.disabled_at IS NOT NULL)`, HashToken(token)).Scan(&disabled)
	return err == nil && disabled
}

// refuseAgent answers an agent whose token did not resolve. A disabled
// account's device gets 403 account-disabled rather than 401: an agent stops
// for good on 401, while on any other refusal it keeps polling every couple of
// seconds, so it comes back by itself once the account is enabled again, with
// no one having to restart it on the device.
func (r *Relay) refuseAgent(w http.ResponseWriter, req *http.Request) {
	if store, ok := r.ts.(interface{ TokenAccountDisabled(string) bool }); ok {
		if token, ok := admission.Token(req); ok && store.TokenAccountDisabled(token) {
			http.Error(w, accountDisabledBody, http.StatusForbidden)
			return
		}
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

func (p *PGStore) AccountDisabled(namespace string) (*time.Time, bool, error) {
	var at sql.NullTime
	err := p.db.QueryRow(`SELECT disabled_at FROM users WHERE namespace = $1`, namespace).Scan(&at)
	return disabledResult(at, err)
}

func (p *PGStore) SetAccountDisabled(namespace string, disabled bool) (*time.Time, bool, error) {
	q := `UPDATE users SET disabled_at = NULL WHERE namespace = $1 RETURNING disabled_at`
	if disabled {
		// Disabling twice keeps the first time: that is when it happened.
		q = `UPDATE users SET disabled_at = COALESCE(disabled_at, now()) WHERE namespace = $1 RETURNING disabled_at`
	}
	var at sql.NullTime
	err := p.db.QueryRow(q, namespace).Scan(&at)
	return disabledResult(at, err)
}

func disabledResult(at sql.NullTime, err error) (*time.Time, bool, error) {
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !at.Valid {
		return nil, true, nil
	}
	return &at.Time, true, nil
}

// adminAccountDisable is the operator's switch. GET ?namespace= reports the
// flag; POST {"namespace","disabled"} sets or clears it, and setting it also
// ends everything the account has open right now.
func (r *Relay) adminAccountDisable(w http.ResponseWriter, req *http.Request) {
	if !r.requireAdminStore(w, req) {
		return
	}
	store, ok := r.admin.(AccountDisabler)
	if !ok {
		http.Error(w, "this admin store cannot disable accounts", http.StatusServiceUnavailable)
		return
	}
	var ns string
	var set *bool
	switch req.Method {
	case http.MethodGet:
		ns = req.URL.Query().Get("namespace")
	case http.MethodPost:
		var body struct {
			Namespace string `json:"namespace"`
			Disabled  *bool  `json:"disabled"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.Disabled == nil {
			http.Error(w, `body must be {"namespace":"…","disabled":true|false}`, http.StatusBadRequest)
			return
		}
		ns, set = body.Namespace, body.Disabled
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ns = strings.TrimSpace(ns)
	if ns == "" {
		http.Error(w, "namespace required", http.StatusBadRequest)
		return
	}
	if r.portalNS != "" && ns == r.portalNS {
		http.Error(w, "the portal's namespace cannot be disabled", http.StatusBadRequest)
		return
	}
	var (
		at    *time.Time
		found bool
		err   error
	)
	if set == nil {
		at, found, err = store.AccountDisabled(ns)
	} else {
		at, found, err = store.SetAccountDisabled(ns, *set)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "no such account", http.StatusNotFound)
		return
	}
	out := map[string]any{"namespace": ns, "disabled": at != nil, "disabled_at": at}
	if set != nil && *set {
		sessions, devices := r.disconnectNamespace(ns)
		out["sessions_closed"], out["devices_disconnected"] = sessions, devices
		if r.audit != nil {
			r.audit.Audit(ns, "", "account disabled")
		}
	} else if set != nil && r.audit != nil {
		r.audit.Audit(ns, "", "account enabled")
	}
	writeJSON(w, out)
}

// disconnectNamespace ends everything live that belongs to ns: the sessions it
// dialed, the sessions into its devices (from any account, the portal's
// included), and its devices' control channels. Its credentials are refused
// before this runs, so nothing it ends can reconnect.
func (r *Relay) disconnectNamespace(ns string) (sessions, devices int) {
	var held []*accessLease
	r.leaseMu.Lock()
	for _, l := range r.leases {
		owner, _, _ := strings.Cut(l.target, "/")
		if l.access.Namespace == ns || owner == ns {
			held = append(held, l)
		}
	}
	r.leaseMu.Unlock()
	for _, l := range held {
		l.close()
	}

	var kick []*agentConn
	r.mu.Lock()
	for _, a := range r.agents {
		if a.ns == ns {
			kick = append(kick, a)
		}
	}
	r.mu.Unlock()
	for _, a := range kick {
		if a.kick != nil {
			go a.kick()
		}
	}

	r.hmu.Lock()
	for key, a := range r.hagents {
		if a.ns == ns {
			// Its parked poll wakes and ends; the next one is refused.
			delete(r.hagents, key)
			close(a.changed)
			a.changed = make(chan struct{})
			devices++
		}
	}
	r.hmu.Unlock()
	return len(held), devices + len(kick)
}
