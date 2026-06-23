// Package portal is the team web app (humans, behind thunderbox Feishu SSO). It
// identifies the logged-in user from an SSO-injected request header, maps them to
// a namespace, and lets them issue/revoke access tokens, view their devices and
// shared-access (ACL) grants, and read the relay audit. It shares the relay's
// Postgres. Tokens are stored hashed with the same scheme the relay resolves.
package portal

import (
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"wanctl/internal/relay"

	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed index.html
var assets embed.FS

// Server is the portal web app.
type Server struct {
	db         *sql.DB
	userHeader string
}

// New opens the shared Postgres and configures the identity header.
func New(dsn, userHeader string) (*Server, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if userHeader == "" {
		userHeader = "X-Forwarded-User"
	}
	return &Server{db: db, userHeader: userHeader}, nil
}

// Handler returns the portal mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/whoami", s.handleWhoami)
	mux.HandleFunc("/api/me", s.handleMe)
	mux.HandleFunc("/api/tokens", s.handleTokens)
	mux.HandleFunc("/api/tokens/revoke", s.handleTokenRevoke)
	mux.HandleFunc("/api/devices", s.handleDevices)
	mux.HandleFunc("/api/acl", s.handleACL)
	mux.HandleFunc("/api/acl/revoke", s.handleACLRevoke)
	mux.HandleFunc("/api/audit", s.handleAudit)
	return mux
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, _ := assets.ReadFile("index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

// handleWhoami dumps request headers so we can discover which one carries the
// SSO identity. Safe to leave in (shows only the caller's own headers).
func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "configured identity header: %q\nresolved identity: %q\n\nall request headers:\n", s.userHeader, r.Header.Get(s.userHeader))
	for k, v := range r.Header {
		fmt.Fprintf(w, "  %s: %s\n", k, strings.Join(v, ", "))
	}
}

// identity returns the SSO-provided identity (header value), or "".
func (s *Server) identity(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get(s.userHeader))
}

// deriveNS turns an identity (often an email) into a namespace slug.
func deriveNS(identity string) string {
	s := identity
	if i := strings.Index(s, "@"); i > 0 {
		s = s[:i]
	}
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// resolveUser maps an identity to a namespace, creating/linking the users row.
func (s *Server) resolveUser(identity string) (string, error) {
	var ns string
	err := s.db.QueryRow(`SELECT namespace FROM users WHERE feishu_open_id = $1`, identity).Scan(&ns)
	if err == nil {
		return ns, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	ns = deriveNS(identity)
	// Link to an existing namespace row if present, else create.
	err = s.db.QueryRow(
		`INSERT INTO users (feishu_open_id, namespace, name) VALUES ($1,$2,$3)
		   ON CONFLICT (namespace) DO UPDATE SET feishu_open_id = EXCLUDED.feishu_open_id
		   RETURNING namespace`,
		identity, ns, identity,
	).Scan(&ns)
	return ns, err
}

// requireNS resolves the caller's namespace or writes a 401 and returns ok=false.
func (s *Server) requireNS(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := s.identity(r)
	if id == "" {
		http.Error(w, "no SSO identity header ("+s.userHeader+"); open /whoami to find the right header", http.StatusUnauthorized)
		return "", false
	}
	ns, err := s.resolveUser(id)
	if err != nil {
		http.Error(w, "resolve user: "+err.Error(), http.StatusInternalServerError)
		return "", false
	}
	return ns, true
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.requireNS(w, r)
	if !ok {
		return
	}
	writeJSON(w, map[string]any{"identity": s.identity(r), "namespace": ns})
}

func (s *Server) handleTokens(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.requireNS(w, r)
	if !ok {
		return
	}
	if r.Method == "POST" {
		var body struct {
			Label string `json:"label"`
			Days  int    `json:"days"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		raw := "wanctl_" + randHex(20)
		var expires any
		if body.Days > 0 {
			expires = time.Now().AddDate(0, 0, body.Days)
		}
		_, err := s.db.Exec(
			`INSERT INTO tokens (namespace, kind, hash, label, expires_at) VALUES ($1,'access',$2,$3,$4)`,
			ns, relay.HashToken(raw), body.Label, expires,
		)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"token": raw}) // shown once
		return
	}
	rows, err := s.db.Query(
		`SELECT id, COALESCE(label,''), created_at, expires_at, revoked_at
		   FROM tokens WHERE namespace = $1 ORDER BY id DESC`, ns)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var out []map[string]any
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
	writeJSON(w, map[string]any{"tokens": out})
}

func (s *Server) handleTokenRevoke(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.requireNS(w, r)
	if !ok {
		return
	}
	var body struct {
		ID int `json:"id"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	_, err := s.db.Exec(
		`UPDATE tokens SET revoked_at = now() WHERE id = $1 AND namespace = $2 AND revoked_at IS NULL`,
		body.ID, ns)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.requireNS(w, r)
	if !ok {
		return
	}
	rows, err := s.db.Query(
		`SELECT name, COALESCE(fingerprint,''), last_seen FROM devices WHERE owner_namespace = $1 ORDER BY name`, ns)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var name, fp string
		var seen sql.NullTime
		rows.Scan(&name, &fp, &seen)
		out = append(out, map[string]any{"name": name, "fingerprint": fp, "last_seen": nullTime(seen)})
	}
	writeJSON(w, map[string]any{"devices": out})
}

func (s *Server) handleACL(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.requireNS(w, r)
	if !ok {
		return
	}
	if r.Method == "POST" {
		var body struct {
			Device  string `json:"device"`
			Grantee string `json:"grantee"`
			Perms   string `json:"perms"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Perms == "" {
			body.Perms = "exec,read,write"
		}
		_, err := s.db.Exec(
			`INSERT INTO acl (owner_namespace, device, grantee_namespace, perms) VALUES ($1,$2,$3,$4)`,
			ns, body.Device, body.Grantee, body.Perms)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	rows, err := s.db.Query(
		`SELECT id, device, grantee_namespace, perms, created_at FROM acl
		   WHERE owner_namespace = $1 AND revoked_at IS NULL ORDER BY id DESC`, ns)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id int
		var device, grantee, perms string
		var created time.Time
		rows.Scan(&id, &device, &grantee, &perms, &created)
		out = append(out, map[string]any{"id": id, "device": device, "grantee": grantee, "perms": perms, "created_at": created})
	}
	writeJSON(w, map[string]any{"acl": out})
}

func (s *Server) handleACLRevoke(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.requireNS(w, r)
	if !ok {
		return
	}
	var body struct {
		ID int `json:"id"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	_, err := s.db.Exec(`UPDATE acl SET revoked_at = now() WHERE id = $1 AND owner_namespace = $2`, body.ID, ns)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.requireNS(w, r)
	if !ok {
		return
	}
	rows, err := s.db.Query(
		`SELECT ts, device, event FROM audit WHERE namespace = $1 ORDER BY id DESC LIMIT 100`, ns)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var ts time.Time
		var device, event string
		rows.Scan(&ts, &device, &event)
		out = append(out, map[string]any{"ts": ts, "device": device, "event": event})
	}
	writeJSON(w, map[string]any{"audit": out})
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
