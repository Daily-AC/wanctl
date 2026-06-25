package relay

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// Admin endpoints let the portal (which has no DB of its own) manage tokens,
// ACLs, devices, and read audit — all scoped to a namespace it has authenticated
// via SSO. They are gated by a shared secret header so the public relay does not
// expose them to anyone. The relay owns the Postgres; the portal is a thin proxy.

// SetAdminSecret enables /admin/* when secret is non-empty.
func (r *Relay) SetAdminSecret(secret string) { r.adminSecret = secret }

func (r *Relay) adminOK(req *http.Request) bool {
	if r.adminSecret == "" || r.admin == nil {
		return false
	}
	got := req.Header.Get("X-Admin-Secret")
	return subtle.ConstantTimeCompare([]byte(got), []byte(r.adminSecret)) == 1
}

func (r *Relay) registerAdmin(mux *http.ServeMux) {
	mux.HandleFunc("/admin/resolve-user", r.adminResolveUser)
	mux.HandleFunc("/admin/tokens", r.adminTokens)
	mux.HandleFunc("/admin/tokens/issue", r.adminTokenIssue)
	mux.HandleFunc("/admin/tokens/revoke", r.adminTokenRevoke)
	mux.HandleFunc("/admin/devices", r.adminDevices)
	mux.HandleFunc("/admin/devices/remove", r.adminDeviceRemove)
	mux.HandleFunc("/admin/acl", r.adminACL)
	mux.HandleFunc("/admin/acl/revoke", r.adminACLRevoke)
	mux.HandleFunc("/admin/audit", r.adminAudit)
	mux.HandleFunc("/admin/enroll/mint", r.handleEnrollMint)
	mux.HandleFunc("/admin/docs/groups", r.adminDocsGroup)
	mux.HandleFunc("/admin/docs/groups/delete", r.adminDocsGroupDelete)
	mux.HandleFunc("/admin/docs/articles", r.adminDocsArticle)
	mux.HandleFunc("/admin/docs/articles/delete", r.adminDocsArticleDelete)
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

func (r *Relay) adminResolveUser(w http.ResponseWriter, req *http.Request) {
	if !r.adminOK(req) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var body struct{ Identity string }
	json.NewDecoder(req.Body).Decode(&body)
	ns, err := r.admin.ResolveUser(body.Identity)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"namespace": ns})
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
		d["online"] = r.deviceLive(ns, name)
	}
	writeJSON(w, map[string]any{"devices": out})
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
	ns = deriveNS(identity)
	err = p.db.QueryRow(
		`INSERT INTO users (feishu_open_id, namespace, name) VALUES ($1,$2,$3)
		   ON CONFLICT (namespace) DO UPDATE SET feishu_open_id = EXCLUDED.feishu_open_id
		   RETURNING namespace`,
		identity, ns, identity,
	).Scan(&ns)
	return ns, err
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
		`SELECT name, COALESCE(fingerprint,''), last_seen FROM devices WHERE owner_namespace = $1 ORDER BY name`, namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var name, fp string
		var seen sql.NullTime
		rows.Scan(&name, &fp, &seen)
		out = append(out, map[string]any{"name": name, "fingerprint": fp, "last_seen": nullTime(seen)})
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
	_, err := p.db.Exec(
		`INSERT INTO acl (owner_namespace, device, grantee_namespace, perms) VALUES ($1,$2,$3,$4)`,
		namespace, device, grantee, perms)
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
