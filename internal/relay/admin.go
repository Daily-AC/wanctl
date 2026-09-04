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
	mux.HandleFunc("/admin/invites", r.adminInvites)
	mux.HandleFunc("/admin/invites/revoke", r.adminInviteRevoke)
	mux.HandleFunc("/admin/users", r.adminUsers)
	mux.HandleFunc("/admin/users/lookup", r.adminUserLookup)
	mux.HandleFunc("/admin/friends", r.adminFriends)
	mux.HandleFunc("/admin/friends/request", r.adminFriendRequest)
	mux.HandleFunc("/admin/friends/accept", r.adminFriendAccept)
	mux.HandleFunc("/admin/friends/decline", r.adminFriendDecline)
	mux.HandleFunc("/admin/friends/remove", r.adminFriendRemove)
	mux.HandleFunc("/admin/tokens/resolve", r.adminTokenResolve)
	mux.HandleFunc("/admin/tokens", r.adminTokens)
	mux.HandleFunc("/admin/tokens/issue", r.adminTokenIssue)
	mux.HandleFunc("/admin/tokens/revoke", r.adminTokenRevoke)
	mux.HandleFunc("/admin/devices", r.adminDevices)
	mux.HandleFunc("/admin/devices/alias", r.adminDeviceAlias)
	mux.HandleFunc("/admin/devices/lark", r.adminDevicesLark)
	mux.HandleFunc("/admin/devices/notify", r.adminDeviceNotify)
	mux.HandleFunc("/admin/notify", r.adminNotify)
	mux.HandleFunc("/admin/notify/test", r.adminNotifyTest)
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
	if !r.requireAdminStore(w, req) {
		return
	}
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Identity   string `json:"identity"`
		Provider   string `json:"provider"`
		Subject    string `json:"subject"`
		Login      string `json:"login"`
		Name       string `json:"name"`
		InviteCode string `json:"invite_code"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if body.Identity != "" {
		body.Provider = "header"
		body.Subject = body.Identity
	}
	ns, role, err := r.admin.ResolveIdentity(
		body.Provider, body.Subject, body.Login, body.Name, body.InviteCode, r.portalNS,
	)
	if err != nil {
		if errors.Is(err, ErrPendingInvite) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, "pending-invite")
			return
		}
		if errors.Is(err, ErrNamespaceConflict) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"namespace": ns, "role": role})
}

func (r *Relay) requireAdminStore(w http.ResponseWriter, req *http.Request) bool {
	if !r.secretOK(req) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	if r.admin == nil {
		http.Error(w, "Postgres admin store is not configured", http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (r *Relay) adminInvites(w http.ResponseWriter, req *http.Request) {
	if !r.requireAdminStore(w, req) {
		return
	}
	switch req.Method {
	case http.MethodPost:
		var body struct {
			GitHubLogin string `json:"github_login"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		invite, code, err := r.admin.CreateInvite(body.GitHubLogin)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if code != "" {
			writeJSON(w, map[string]any{"id": invite.ID, "code": code})
			return
		}
		writeJSON(w, map[string]any{"id": invite.ID, "github_login": invite.GitHubLogin})
	case http.MethodGet:
		invites, err := r.admin.ListInvites()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, invites)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (r *Relay) adminInviteRevoke(w http.ResponseWriter, req *http.Request) {
	if !r.requireAdminStore(w, req) {
		return
	}
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID int `json:"id"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	revoked, err := r.admin.RevokeInvite(body.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !revoked {
		http.Error(w, "invite not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
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

func (r *Relay) adminDeviceAlias(w http.ResponseWriter, req *http.Request) {
	if !r.adminOK(req) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !requireMethod(w, req, http.MethodPost) {
		return
	}
	var body struct {
		Namespace string `json:"namespace"`
		Device    string `json:"device"`
		Alias     string `json:"alias"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeErrorToken(w, http.StatusBadRequest, ErrAliasInvalid.Error())
		return
	}
	if body.Namespace == "" || body.Device == "" {
		writeErrorToken(w, http.StatusBadRequest, ErrAliasInvalid.Error())
		return
	}
	alias, err := normalizeDeviceAlias(body.Alias)
	if err != nil {
		writeErrorToken(w, http.StatusBadRequest, ErrAliasInvalid.Error())
		return
	}
	store, ok := r.admin.(DeviceAliasStore)
	if !ok {
		http.Error(w, "device aliases are not supported by the admin store", http.StatusServiceUnavailable)
		return
	}
	out, err := store.SetDeviceAlias(body.Namespace, body.Device, alias)
	switch {
	case errors.Is(err, ErrAliasInvalid):
		writeErrorToken(w, http.StatusBadRequest, ErrAliasInvalid.Error())
	case errors.Is(err, ErrAliasTaken):
		writeErrorToken(w, http.StatusConflict, ErrAliasTaken.Error())
	case errors.Is(err, ErrAliasShadowsDevice):
		writeErrorToken(w, http.StatusConflict, ErrAliasShadowsDevice.Error())
	case errors.Is(err, ErrDeviceNotFound):
		writeErrorToken(w, http.StatusNotFound, ErrDeviceNotFound.Error())
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	default:
		writeJSON(w, out)
	}
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
		if strings.TrimSpace(body.Namespace) == "" || strings.TrimSpace(body.Device) == "" ||
			strings.TrimSpace(body.Grantee) == "" || strings.TrimSpace(body.Perms) == "" {
			http.Error(w, "namespace, device, grantee and perms are required", http.StatusBadRequest)
			return
		}
		if err := r.admin.AddACL(body.Namespace, body.Device, body.Grantee, body.Perms); err != nil {
			if errors.Is(err, ErrNotFriends) {
				writeErrorToken(w, http.StatusForbidden, ErrNotFriends.Error())
				return
			}
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
	ResolveIdentity(provider, subject, login, name, inviteCode, reservedNS string) (ns, role string, err error)
	CreateInvite(githubLogin string) (Invite, string, error)
	ListInvites() ([]Invite, error)
	RevokeInvite(id int) (bool, error)
	UpsertDevice(namespace, name, fingerprint string)
	IssueToken(namespace, label string, days int) (string, error)
	ListTokens(namespace string) ([]map[string]any, error)
	RevokeToken(namespace string, id int) error
	ListDevices(namespace string) ([]map[string]any, error)
	ListLarkApproval(namespace string) ([]DeviceLarkApproval, error)
	UpsertLarkApproval(DeviceLarkApproval) (DeviceLarkApproval, error)
	ListUsers() ([]string, error)
	LookupUser(namespace string) (bool, error)
	FriendRequest(requester, addressee, reservedNS string) (string, error)
	FriendAccept(namespace, requester string) error
	FriendDecline(namespace, requester string) error
	FriendRemove(namespace, other string) error
	ListFriends(namespace string) ([]Friend, error)
	IsFriend(a, b string) (bool, error)
	RemoveDevice(namespace, device string) error
	ListACL(namespace string) ([]map[string]any, error)
	ListReceivedACL(namespace string) ([]ReceivedShare, error)
	AddACL(namespace, device, grantee, perms string) error
	GrantACL(namespace, device, grantee, perms string) (int, error)
	RevokeACL(namespace string, id int) error
	RevokeACLMatch(namespace string, id int, device, grantee string) (bool, error)
	ListAudit(namespace string) ([]map[string]any, error)
	RoleForNamespace(namespace string) (string, error)
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

// ErrNamespaceConflict means an identity derives to a namespace already
// owned by a different immutable identity. Callers should surface this as a
// conflict, never relink the existing namespace.
var ErrNamespaceConflict = errors.New("derived namespace is already owned by another identity")

// ErrPendingInvite means a GitHub identity has not been admitted yet.
var ErrPendingInvite = errors.New("pending-invite")

// Invite is the public representation of an admission invitation. It never
// contains the stored code hash or a raw invite code.
type Invite struct {
	ID              int        `json:"id"`
	GitHubLogin     string     `json:"github_login"`
	CreatedAt       time.Time  `json:"created_at"`
	UsedAt          *time.Time `json:"used_at"`
	UsedByNamespace string     `json:"used_by_namespace"`
	HasCode         bool       `json:"has_code"`
}

type identityQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

// ResolveUser maps an SSO identity to a namespace, creating/linking the row.
func (p *PGStore) ResolveUser(identity string) (string, error) {
	ns, _, err := p.ResolveIdentity("header", identity, "", "", "", "")
	return ns, err
}

// ResolveIdentity maps an immutable provider identity to a namespace, creating
// the user when the provider's admission policy permits it.
func (p *PGStore) ResolveIdentity(provider, subject, login, name, inviteCode, reservedNS string) (string, string, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	subject = strings.TrimSpace(subject)
	login = strings.TrimSpace(login)
	name = strings.TrimSpace(name)
	if subject == "" {
		return "", "", sql.ErrNoRows
	}

	if ns, role, err := lookupIdentity(p.db, provider, subject); err == nil {
		return ns, role, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", "", err
	}

	switch provider {
	case "header":
		preferredNS := deriveNS(subject)
		if err := guardNamespace(preferredNS, reservedNS); err != nil {
			return "", "", err
		}
		return insertIdentity(p.db, provider, subject, preferredNS, subject, "user")
	case "github":
		if !decimalString(subject) {
			return "", "", fmt.Errorf("invalid GitHub subject %q", subject)
		}
		preferredNS := strings.ToLower(login)
		if !validGitHubLogin(preferredNS) {
			return "", "", fmt.Errorf("invalid GitHub login %q", login)
		}
		if err := guardNamespace(preferredNS, reservedNS); err != nil {
			return "", "", err
		}
		return p.resolveGitHubIdentity(subject, preferredNS, name, inviteCode)
	default:
		return "", "", fmt.Errorf("unsupported identity provider %q", provider)
	}
}

func lookupIdentity(q identityQuerier, provider, subject string) (string, string, error) {
	var ns, role string
	err := q.QueryRow(
		`SELECT namespace, role FROM users WHERE provider = $1 AND provider_subject = $2`,
		provider, subject,
	).Scan(&ns, &role)
	return ns, role, err
}

func insertIdentity(q identityQuerier, provider, subject, namespace, name, role string) (string, string, error) {
	var insertedNS, insertedRole string
	err := q.QueryRow(
		`INSERT INTO users (provider, provider_subject, namespace, name, role)
		 VALUES ($1,$2,$3,NULLIF($4,''),$5)
		 ON CONFLICT (namespace) DO NOTHING
		 RETURNING namespace, role`,
		provider, subject, namespace, name, role,
	).Scan(&insertedNS, &insertedRole)
	if errors.Is(err, sql.ErrNoRows) {
		// A concurrent request for the same immutable identity is idempotent:
		// the winner inserted the row after our initial lookup. A different
		// identity deriving to the same namespace is a hard conflict.
		existingNS, existingRole, lookupErr := lookupIdentity(q, provider, subject)
		if lookupErr == nil {
			return existingNS, existingRole, nil
		}
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			return "", "", fmt.Errorf("recheck identity after namespace conflict: %w", lookupErr)
		}
		return "", "", fmt.Errorf("%w: %q", ErrNamespaceConflict, namespace)
	}
	return insertedNS, insertedRole, err
}

func (p *PGStore) resolveGitHubIdentity(subject, namespace, name, inviteCode string) (ns, role string, err error) {
	tx, err := p.db.Begin()
	if err != nil {
		return "", "", err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.Exec(`SELECT pg_advisory_xact_lock(hashtext('wanctl_admission'))`); err != nil {
		return "", "", err
	}
	if ns, role, err = lookupIdentity(tx, "github", subject); err == nil {
		if err = tx.Commit(); err != nil {
			return "", "", err
		}
		return ns, role, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", "", err
	}

	var hasAdmin bool
	if err = tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM users WHERE role = 'admin')`).Scan(&hasAdmin); err != nil {
		return "", "", err
	}
	inviteID := 0
	role = "admin"
	if hasAdmin {
		role = "user"
		if inviteCode != "" {
			err = tx.QueryRow(
				`SELECT id FROM invites WHERE code_hash = $1 AND used_at IS NULL FOR UPDATE`,
				HashToken(inviteCode),
			).Scan(&inviteID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return "", "", err
			}
		}
		if inviteID == 0 {
			err = tx.QueryRow(
				`SELECT id FROM invites
				  WHERE lower(github_login) = lower($1) AND used_at IS NULL
				  FOR UPDATE`, namespace,
			).Scan(&inviteID)
		}
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", ErrPendingInvite
		}
		if err != nil {
			return "", "", err
		}
		if inviteID == 0 {
			return "", "", ErrPendingInvite
		}
	}

	profileName := name
	if profileName == "" {
		profileName = namespace
	}
	if ns, role, err = insertIdentity(tx, "github", subject, namespace, profileName, role); err != nil {
		return "", "", err
	}
	if inviteID != 0 {
		result, updateErr := tx.Exec(
			`UPDATE invites SET used_at = now(), used_by_namespace = $2
			  WHERE id = $1 AND used_at IS NULL`, inviteID, ns,
		)
		if updateErr != nil {
			return "", "", updateErr
		}
		updated, updateErr := result.RowsAffected()
		if updateErr != nil {
			return "", "", updateErr
		}
		if updated != 1 {
			return "", "", ErrPendingInvite
		}
	}
	if err = tx.Commit(); err != nil {
		return "", "", err
	}
	return ns, role, nil
}

func guardNamespace(namespace, reservedNS string) error {
	if namespace == "" {
		return fmt.Errorf("%w: namespace is empty", ErrNamespaceConflict)
	}
	if reservedNS != "" && strings.EqualFold(namespace, strings.TrimSpace(reservedNS)) {
		return fmt.Errorf("%w: %q is reserved", ErrNamespaceConflict, namespace)
	}
	// "portal" is the conventional privileged namespace (WANCTL_PORTAL_NS): a
	// token resolving to it may dial any device. Reserve the literal even when
	// the relay has not configured it yet, so a deployment that opens
	// registration first and wires the portal second cannot have the name
	// squatted into a super-user account in between.
	if strings.EqualFold(namespace, "portal") {
		return fmt.Errorf("%w: %q is reserved", ErrNamespaceConflict, namespace)
	}
	return nil
}

func validGitHubLogin(login string) bool {
	if login == "" {
		return false
	}
	for _, c := range login {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

func decimalString(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (p *PGStore) CreateInvite(githubLogin string) (Invite, string, error) {
	githubLogin = strings.ToLower(strings.TrimSpace(githubLogin))
	var invite Invite
	var code string
	if githubLogin != "" {
		if !validGitHubLogin(githubLogin) {
			return invite, "", fmt.Errorf("invalid GitHub login %q", githubLogin)
		}
		err := p.db.QueryRow(
			`INSERT INTO invites (github_login) VALUES ($1)
			 RETURNING id, github_login, created_at`, githubLogin,
		).Scan(&invite.ID, &invite.GitHubLogin, &invite.CreatedAt)
		return invite, "", err
	}
	code = "winv_" + randHex(12)
	err := p.db.QueryRow(
		`INSERT INTO invites (code_hash) VALUES ($1)
		 RETURNING id, created_at`, HashToken(code),
	).Scan(&invite.ID, &invite.CreatedAt)
	if err != nil {
		return Invite{}, "", err
	}
	invite.HasCode = true
	return invite, code, nil
}

func (p *PGStore) ListInvites() ([]Invite, error) {
	rows, err := p.db.Query(
		`SELECT id, COALESCE(github_login,''), created_at, used_at,
		        COALESCE(used_by_namespace,''), code_hash IS NOT NULL
		   FROM invites ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	invites := []Invite{}
	for rows.Next() {
		var invite Invite
		var usedAt sql.NullTime
		if err := rows.Scan(
			&invite.ID, &invite.GitHubLogin, &invite.CreatedAt, &usedAt,
			&invite.UsedByNamespace, &invite.HasCode,
		); err != nil {
			return nil, err
		}
		if usedAt.Valid {
			invite.UsedAt = &usedAt.Time
		}
		invites = append(invites, invite)
	}
	return invites, rows.Err()
}

func (p *PGStore) RevokeInvite(id int) (bool, error) {
	result, err := p.db.Exec(`DELETE FROM invites WHERE id = $1 AND used_at IS NULL`, id)
	if err != nil {
		return false, err
	}
	deleted, err := result.RowsAffected()
	return deleted == 1, err
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
		`SELECT name, alias, fingerprint, last_seen, owner_namespace, shared, perms
		   FROM (
		     SELECT d.name,
		            COALESCE(d.alias,'') AS alias,
		            COALESCE(d.fingerprint,'') AS fingerprint,
		            d.last_seen,
		            d.owner_namespace,
		            false AS shared,
		            '' AS perms
		       FROM devices d
		      WHERE d.owner_namespace = $1
		     UNION ALL
		     SELECT d.name,
		            COALESCE(d.alias,'') AS alias,
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
		var name, alias, fp, owner, perms string
		var seen sql.NullTime
		var shared bool
		rows.Scan(&name, &alias, &fp, &seen, &owner, &shared, &perms)
		row := map[string]any{
			"name": name, "alias": alias, "fingerprint": fp, "last_seen": nullTime(seen),
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

// RoleForNamespace reports the admission role of the user owning a namespace.
// sql.ErrNoRows when no user record exists: tokens can outlive their user, and
// pre-admission deployments never had one.
func (p *PGStore) RoleForNamespace(namespace string) (string, error) {
	var role string
	err := p.db.QueryRow(`SELECT role FROM users WHERE namespace = $1`, namespace).Scan(&role)
	return role, err
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
	_, err := p.GrantACL(namespace, device, grantee, perms)
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
