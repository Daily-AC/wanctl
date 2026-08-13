package relay

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"wanctl/internal/serverlog"
	"wanctl/internal/sessionauth"
)

// Admin endpoints let the portal (which has no DB of its own) manage tokens,
// ACLs, devices, and read audit — all scoped to a namespace it has authenticated
// via SSO. They are gated by a shared secret header so the public relay does not
// expose them to anyone. The relay owns the Postgres; the portal is a thin proxy.

// SetAdminSecret enables /admin/* when secret is non-empty.
func (r *Relay) SetAdminSecret(secret string) { r.adminSecret = secret }

func (r *Relay) adminOK(req *http.Request) bool {
	return r.admin != nil && r.secretOK(req)
}

// secretOK checks only the shared admin secret (no Postgres backend needed).
func (r *Relay) secretOK(req *http.Request) bool {
	if r.adminSecret == "" {
		return false
	}
	got := req.Header.Get("X-Admin-Secret")
	return subtle.ConstantTimeCompare([]byte(got), []byte(r.adminSecret)) == 1
}

func (r *Relay) registerAdmin(mux *http.ServeMux) {
	mux.HandleFunc("/admin/resolve-user", r.adminResolveUser)
	mux.HandleFunc("/admin/users", r.adminUsers)
	mux.HandleFunc("/admin/tokens/resolve", r.adminTokenResolve)
	mux.HandleFunc("/admin/tokens", r.adminTokens)
	mux.HandleFunc("/admin/tokens/issue", r.adminTokenIssue)
	mux.HandleFunc("/admin/tokens/revoke", r.adminTokenRevoke)
	mux.HandleFunc("/admin/devices", r.adminDevices)
	mux.HandleFunc("/admin/devices/lark", r.adminDevicesLark)
	mux.HandleFunc("/admin/devices/remove", r.adminDeviceRemove)
	mux.HandleFunc("/admin/acl", r.adminACL)
	mux.HandleFunc("/admin/acl/revoke", r.adminACLRevoke)
	mux.HandleFunc("/admin/audit", r.adminAudit)
	mux.HandleFunc("/admin/logs", r.adminLogs)
	mux.HandleFunc("/admin/enroll/mint", r.handleEnrollMint)
	mux.HandleFunc("/admin/docs/groups", r.adminDocsGroup)
	mux.HandleFunc("/admin/docs/groups/delete", r.adminDocsGroupDelete)
	mux.HandleFunc("/admin/docs/articles", r.adminDocsArticle)
	mux.HandleFunc("/admin/docs/articles/delete", r.adminDocsArticleDelete)
}

func (r *Relay) adminLogs(w http.ResponseWriter, req *http.Request) {
	if !r.secretOK(req) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if req.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q, err := serverlog.ParseQuery(req.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if q.Service != "relay" {
		http.Error(w, "service must be relay", http.StatusBadRequest)
		return
	}
	if r.logs == nil {
		http.Error(w, "relay log buffer is not configured", http.StatusServiceUnavailable)
		return
	}
	serverlog.WriteJSON(w, r.logs.Read(q))
}

// adminDocsGroup is the admin-secret-gated mirror of POST /docs/groups. The
// author namespace comes from the caller-supplied "author" JSON field (the
// portal injects the SSO-resolved namespace there).
func (r *Relay) adminDocsGroup(w http.ResponseWriter, req *http.Request) {
	if !r.adminOK(req) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	author := adminAuthorFromBody(req)
	r.docsUpsertGroup(w, req, author)
}

func (r *Relay) adminDocsGroupDelete(w http.ResponseWriter, req *http.Request) {
	if !r.adminOK(req) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	r.docsDeleteGroup(w, req)
}

func (r *Relay) adminDocsArticle(w http.ResponseWriter, req *http.Request) {
	if !r.adminOK(req) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	author := adminAuthorFromBody(req)
	r.docsUpsertArticle(w, req, author)
}

func (r *Relay) adminDocsArticleDelete(w http.ResponseWriter, req *http.Request) {
	if !r.adminOK(req) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	r.docsDeleteArticle(w, req)
}

// adminAuthorFromBody peeks at the "author" JSON field without consuming the
// body (it re-wraps it for the downstream handler). Empty if absent or
// unparseable — the downstream handler will still succeed; author just stays
// blank in the audit row.
func adminAuthorFromBody(req *http.Request) string {
	if req.Body == nil {
		return ""
	}
	var probe map[string]any
	buf := new(strings.Builder)
	_, _ = io.Copy(buf, req.Body)
	raw := buf.String()
	req.Body = io.NopCloser(strings.NewReader(raw))
	json.Unmarshal([]byte(raw), &probe)
	if a, _ := probe["author"].(string); a != "" {
		return a
	}
	return ""
}

// adminTokenResolve resolves a raw token to its namespace. It exists for
// satellite relays (the intranet fast-path relay) whose UpstreamTokenStore
// delegates token auth here, so portal-issued tokens work on every relay
// without sharing the DB. Gated by the same shared secret as the rest of
// /admin/*, but does not require the Postgres admin backend — it only needs
// the token store.
func (r *Relay) adminTokenResolve(w http.ResponseWriter, req *http.Request) {
	if !r.secretOK(req) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var body struct{ Token string }
	json.NewDecoder(req.Body).Decode(&body)
	if body.Token == "" {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}
	ns, ok := r.ts.Resolve(body.Token)
	if !ok {
		http.Error(w, "unknown token", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]string{"namespace": ns})
}

func (r *Relay) adminResolveUser(w http.ResponseWriter, req *http.Request) {
	if !r.adminOK(req) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var body struct{ Identity string }
	json.NewDecoder(req.Body).Decode(&body)
	ns, err := r.admin.ResolveUser(body.Identity)
	if err != nil {
		if errors.Is(err, ErrNamespaceConflict) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"namespace": ns})
}

func (r *Relay) adminUsers(w http.ResponseWriter, req *http.Request) {
	if !r.adminOK(req) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	out, err := r.admin.ListUsers()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"namespaces": out})
}

func (r *Relay) adminTokens(w http.ResponseWriter, req *http.Request) {
	if !r.adminOK(req) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	out, err := r.admin.ListTokens(req.URL.Query().Get("namespace"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"tokens": out})
}

func (r *Relay) adminTokenIssue(w http.ResponseWriter, req *http.Request) {
	if !r.adminOK(req) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var body struct {
		Namespace, Label string
		Days             int
	}
	json.NewDecoder(req.Body).Decode(&body)
	raw, err := r.admin.IssueToken(body.Namespace, body.Label, body.Days)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"token": raw})
}

func (r *Relay) adminTokenRevoke(w http.ResponseWriter, req *http.Request) {
	if !r.adminOK(req) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var body struct {
		Namespace string
		ID        int
	}
	json.NewDecoder(req.Body).Decode(&body)
	if err := r.admin.RevokeToken(body.Namespace, body.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (r *Relay) adminDevices(w http.ResponseWriter, req *http.Request) {
	if !r.adminOK(req) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ns := req.URL.Query().Get("namespace")
	out, err := r.admin.ListDevices(ns)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Mark true liveness from the in-memory registry (matches dial-ability),
	// rather than letting the UI guess from the lagging last_seen timestamp.
	for _, d := range out {
		name, _ := d["name"].(string)
		owner, _ := d["owner"].(string)
		if owner == "" {
			owner = ns
		}
		d["online"] = r.deviceLive(owner, name)
	}
	writeJSON(w, map[string]any{"devices": out})
}

func (r *Relay) adminDevicesLark(w http.ResponseWriter, req *http.Request) {
	if !r.adminOK(req) {
		if r.admin == nil && r.secretOK(req) {
			http.Error(w, "Postgres admin store is not configured", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	switch req.Method {
	case http.MethodGet:
		ns := req.URL.Query().Get("namespace")
		if ns == "" {
			http.Error(w, "namespace required", http.StatusBadRequest)
			return
		}
		out, err := r.admin.ListLarkApproval(ns)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"devices": out})
	case http.MethodPost:
		var body struct {
			Namespace       string `json:"namespace"`
			Device          string `json:"device"`
			ApprovalEnabled bool   `json:"approval_enabled"`
			PairingFromCard bool   `json:"pairing_from_card"`
			NotifyEmail     string `json:"notify_email"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		if body.Namespace == "" || body.Device == "" || body.NotifyEmail == "" {
			http.Error(w, "namespace, device, and notify_email required", http.StatusBadRequest)
			return
		}
		cfg := DeviceLarkApproval{
			Namespace:       body.Namespace,
			Device:          body.Device,
			ApprovalEnabled: body.ApprovalEnabled,
			PairingFromCard: body.PairingFromCard,
			NotifyEmail:     body.NotifyEmail,
		}
		out, err := r.admin.UpsertLarkApproval(cfg)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, out)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// adminDeviceRemove unbinds a device record from a namespace.
func (r *Relay) adminDeviceRemove(w http.ResponseWriter, req *http.Request) {
	if !r.adminOK(req) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var body struct{ Namespace, Device string }
	json.NewDecoder(req.Body).Decode(&body)
	if body.Namespace == "" || body.Device == "" {
		http.Error(w, "namespace and device required", http.StatusBadRequest)
		return
	}
	if err := r.admin.RemoveDevice(body.Namespace, body.Device); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Evict any live registry entry so it disappears immediately.
	key := body.Namespace + "/" + body.Device
	r.hmu.Lock()
	delete(r.hagents, key)
	r.hmu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (r *Relay) adminACL(w http.ResponseWriter, req *http.Request) {
	if !r.adminOK(req) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if req.Method == "POST" {
		var body struct{ Namespace, Device, Grantee, Perms string }
		json.NewDecoder(req.Body).Decode(&body)
		if err := r.admin.AddACL(body.Namespace, body.Device, body.Grantee, body.Perms); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	out, err := r.admin.ListACL(req.URL.Query().Get("namespace"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"acl": out})
}

func (r *Relay) adminACLRevoke(w http.ResponseWriter, req *http.Request) {
	if !r.adminOK(req) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var body struct {
		Namespace string
		ID        int
	}
	json.NewDecoder(req.Body).Decode(&body)
	if err := r.admin.RevokeACL(body.Namespace, body.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (r *Relay) adminAudit(w http.ResponseWriter, req *http.Request) {
	if !r.adminOK(req) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	out, err := r.admin.ListAudit(req.URL.Query().Get("namespace"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"audit": out})
}

// --- PGStore admin operations ---

// AdminStore is the DB surface the admin endpoints need.
type AdminStore interface {
	ResolveUser(identity string) (string, error)
	UpsertDevice(namespace, name, fingerprint string)
	IssueToken(namespace, label string, days int) (string, error)
	ListTokens(namespace string) ([]map[string]any, error)
	RevokeToken(namespace string, id int) error
	ListDevices(namespace string) ([]map[string]any, error)
	ListLarkApproval(namespace string) ([]DeviceLarkApproval, error)
	UpsertLarkApproval(DeviceLarkApproval) (DeviceLarkApproval, error)
	ListUsers() ([]string, error)
	RemoveDevice(namespace, device string) error
	ListACL(namespace string) ([]map[string]any, error)
	AddACL(namespace, device, grantee, perms string) error
	RevokeACL(namespace string, id int) error
	ListAudit(namespace string) ([]map[string]any, error)
}

func deriveNS(identity string) string {
	s := identity
	if i := strings.Index(s, "@"); i > 0 {
		s = s[:i]
	}
	s = strings.ToLower(s)
	var b strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteRune(c)
		} else {
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// ErrNamespaceConflict means an SSO identity derives to a namespace already
// owned by a different immutable identity. Callers should surface this as a
// conflict, never relink the existing namespace.
var ErrNamespaceConflict = errors.New("derived namespace is already owned by another SSO identity")

// ResolveUser maps an SSO identity to a namespace, creating/linking the row.
func (p *PGStore) ResolveUser(identity string) (string, error) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return "", sql.ErrNoRows
	}
	var ns string
	err := p.db.QueryRow(`SELECT namespace FROM users WHERE feishu_open_id = $1`, identity).Scan(&ns)
	if err == nil {
		return ns, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	derivedNS := deriveNS(identity)
	var insertedNS string
	err = p.db.QueryRow(
		`INSERT INTO users (feishu_open_id, namespace, name) VALUES ($1,$2,$3)
		   ON CONFLICT (namespace) DO NOTHING
		   RETURNING namespace`,
		identity, derivedNS, identity,
	).Scan(&insertedNS)
	if errors.Is(err, sql.ErrNoRows) {
		// A concurrent request for the same immutable identity is idempotent:
		// the winner inserted the row after our initial lookup. A different
		// identity deriving to the same namespace is a hard conflict.
		var existingNS string
		lookupErr := p.db.QueryRow(`SELECT namespace FROM users WHERE feishu_open_id = $1`, identity).Scan(&existingNS)
		if lookupErr == nil {
			return existingNS, nil
		}
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			return "", fmt.Errorf("recheck SSO identity after namespace conflict: %w", lookupErr)
		}
		return "", fmt.Errorf("%w: %q", ErrNamespaceConflict, derivedNS)
	}
	return insertedNS, err
}

// UpsertDevice records (or refreshes last_seen for) an online device.
// Best-effort: never blocks registration on a DB hiccup.
func (p *PGStore) UpsertDevice(namespace, name, fingerprint string) {
	_, _ = p.db.Exec(
		`INSERT INTO devices (owner_namespace, name, fingerprint, last_seen) VALUES ($1,$2,NULLIF($3,''),now())
		   ON CONFLICT (owner_namespace, name) DO UPDATE
		     SET last_seen = now(),
		         fingerprint = COALESCE(NULLIF(EXCLUDED.fingerprint,''), devices.fingerprint)`,
		namespace, name, fingerprint)
}

func (p *PGStore) IssueToken(namespace, label string, days int) (string, error) {
	raw := "wanctl_" + randHex(20)
	var expires any
	if days > 0 {
		expires = time.Now().AddDate(0, 0, days)
	}
	_, err := p.db.Exec(
		`INSERT INTO tokens (namespace, kind, hash, label, expires_at) VALUES ($1,'access',$2,$3,$4)`,
		namespace, HashToken(raw), label, expires)
	if err != nil {
		return "", err
	}
	return raw, nil
}

func (p *PGStore) ListTokens(namespace string) ([]map[string]any, error) {
	rows, err := p.db.Query(
		`SELECT id, COALESCE(label,''), created_at, expires_at, revoked_at
		   FROM tokens WHERE namespace = $1 ORDER BY id DESC`, namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int
		var label string
		var created time.Time
		var expires, revoked sql.NullTime
		rows.Scan(&id, &label, &created, &expires, &revoked)
		out = append(out, map[string]any{
			"id": id, "label": label, "created_at": created,
			"expires_at": nullTime(expires), "revoked_at": nullTime(revoked),
		})
	}
	return out, rows.Err()
}

func (p *PGStore) RevokeToken(namespace string, id int) error {
	_, err := p.db.Exec(`UPDATE tokens SET revoked_at = now() WHERE id = $1 AND namespace = $2 AND revoked_at IS NULL`, id, namespace)
	return err
}

func (p *PGStore) ListDevices(namespace string) ([]map[string]any, error) {
	rows, err := p.db.Query(
		`SELECT name, fingerprint, last_seen, owner_namespace, shared, perms
		   FROM (
		     SELECT d.name,
		            COALESCE(d.fingerprint,'') AS fingerprint,
		            d.last_seen,
		            d.owner_namespace,
		            false AS shared,
		            '' AS perms
		       FROM devices d
		      WHERE d.owner_namespace = $1
		     UNION ALL
		     SELECT d.name,
		            COALESCE(d.fingerprint,'') AS fingerprint,
		            d.last_seen,
		            d.owner_namespace,
		            true AS shared,
		            a.perms
		       FROM acl a
		       JOIN devices d
		         ON d.owner_namespace = a.owner_namespace
		        AND d.name = a.device
		      WHERE a.grantee_namespace = $1
		        AND a.revoked_at IS NULL
		   ) visible
		  ORDER BY name, shared, owner_namespace`, namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var name, fp, owner, perms string
		var seen sql.NullTime
		var shared bool
		rows.Scan(&name, &fp, &seen, &owner, &shared, &perms)
		row := map[string]any{
			"name": name, "fingerprint": fp, "last_seen": nullTime(seen),
			"owner": owner, "shared": shared,
		}
		if shared {
			row["perms"] = perms
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	markAmbiguousDevices(out)
	return out, nil
}

func markAmbiguousDevices(devices []map[string]any) {
	counts := map[string]int{}
	for _, d := range devices {
		name, _ := d["name"].(string)
		if name != "" {
			counts[name]++
		}
	}
	for _, d := range devices {
		name, _ := d["name"].(string)
		if counts[name] > 1 {
			d["ambiguous"] = true
		}
	}
}

func (p *PGStore) ListUsers() ([]string, error) {
	rows, err := p.db.Query(`SELECT DISTINCT namespace FROM users WHERE namespace <> '' ORDER BY namespace`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var ns string
		rows.Scan(&ns)
		out = append(out, ns)
	}
	return out, rows.Err()
}

// RemoveDevice unbinds a device from a namespace (and any ACL grants for it).
func (p *PGStore) RemoveDevice(namespace, device string) error {
	if _, err := p.db.Exec(`DELETE FROM devices WHERE owner_namespace=$1 AND name=$2`, namespace, device); err != nil {
		return err
	}
	_, _ = p.db.Exec(`DELETE FROM acl WHERE owner_namespace=$1 AND device=$2`, namespace, device)
	return nil
}

func (p *PGStore) ListACL(namespace string) ([]map[string]any, error) {
	rows, err := p.db.Query(
		`SELECT id, device, grantee_namespace, perms, created_at FROM acl
		   WHERE owner_namespace = $1 AND revoked_at IS NULL ORDER BY id DESC`, namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int
		var device, grantee, perms string
		var created time.Time
		rows.Scan(&id, &device, &grantee, &perms, &created)
		out = append(out, map[string]any{"id": id, "device": device, "grantee": grantee, "perms": perms, "created_at": created})
	}
	return out, rows.Err()
}

func (p *PGStore) AddACL(namespace, device, grantee, perms string) error {
	if perms == "" {
		perms = "exec,read,write"
	}
	caps, err := sessionauth.ParseGrant(perms)
	if err != nil {
		return fmt.Errorf("invalid ACL permissions: %w", err)
	}
	_, err = p.db.Exec(
		`INSERT INTO acl (owner_namespace, device, grantee_namespace, perms) VALUES ($1,$2,$3,$4)`,
		namespace, device, grantee, caps.String())
	return err
}

func (p *PGStore) RevokeACL(namespace string, id int) error {
	_, err := p.db.Exec(`UPDATE acl SET revoked_at = now() WHERE id = $1 AND owner_namespace = $2`, id, namespace)
	return err
}

func (p *PGStore) ListAudit(namespace string) ([]map[string]any, error) {
	rows, err := p.db.Query(
		`SELECT ts, COALESCE(device,''), COALESCE(event,'') FROM audit WHERE namespace = $1 ORDER BY id DESC LIMIT 100`, namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var ts time.Time
		var device, event string
		rows.Scan(&ts, &device, &event)
		out = append(out, map[string]any{"ts": ts, "device": device, "event": event})
	}
	return out, rows.Err()
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func nullTime(t sql.NullTime) any {
	if t.Valid {
		return t.Time
	}
	return nil
}
