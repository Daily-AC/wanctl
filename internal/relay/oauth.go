package relay

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"wanctl/internal/mcpauth"
)

// OAuth 2.1 for the hosted MCP endpoint.
//
// Why this exists: an MCP client that opens a fresh session for every tool
// call — ChatGPT's connector does, by design or by bug — can never stay logged
// in through wanctl_login, because that login lives in a session keyed by
// Mcp-Session-Id. The MCP authorization spec answers this with a bearer token
// the client attaches to every request, so identity stops depending on the
// session at all.
//
// The split: the machine half lives here (metadata, registration, token,
// revocation) because the relay is what has the MCP seed, the database and a
// public origin. The half a human looks at — sign in, read who is asking, say
// yes — lives on the portal, which is what already has GitHub login and the
// namespace a login resolves to. The two talk over the existing admin-secret
// channel. See docs/adr/0008-mcp-oauth-split-portal-relay.md.
const (
	oauthScope        = "wanctl"
	oauthAccessTTL    = time.Hour
	oauthRefreshTTL   = 30 * 24 * time.Hour
	oauthRequestTTL   = 10 * time.Minute
	oauthCodeTTL      = 10 * time.Minute
	oauthMaxRedirects = 5
)

// OAuthClient is a client that registered itself under RFC 7591. There is no
// review step: a public MCP endpoint cannot know its clients in advance, and
// registering buys nothing on its own — a client_id only becomes access when a
// signed-in human approves it on the portal.
type OAuthClient struct {
	ID           string
	SecretHash   string // "" when the client registered with auth method "none"
	Name         string
	RedirectURIs []string
	AuthMethod   string
	CreatedAt    time.Time
}

// OAuthRefresh is a stored refresh token. Grant is the sealed relay token
// (mcpauth.SealGrant), so the row cannot be turned into device access without
// the relay's MCP seed.
type OAuthRefresh struct {
	Hash      string
	ClientID  string
	Namespace string
	Grant     string
	ExpiresAt time.Time
	RevokedAt time.Time
}

// OAuthStore is the durable half. It is separate from AdminStore because a
// relay can run the MCP endpoint without one and because these rows have
// nothing to do with the portal's admin surface.
type OAuthStore interface {
	PutOAuthClient(OAuthClient) error
	OAuthClient(id string) (OAuthClient, bool, error)
	PutOAuthRefresh(OAuthRefresh) error
	OAuthRefresh(hash string) (OAuthRefresh, bool, error)
	RevokeOAuthRefresh(hash string) error
	// RevokeRelayTokenHash revokes the namespace token an OAuth grant minted,
	// addressed by hash because the raw token is never stored in the clear.
	RevokeRelayTokenHash(namespace, hash string) error
}

// oauthAuthzRequest is one browser trip in flight: the client has asked, the
// human has not yet answered. Held in memory with a 10-minute life, because a
// request nobody answered in ten minutes is one the client has already retried.
type oauthAuthzRequest struct {
	id            string
	clientID      string
	clientName    string
	redirectURI   string
	state         string
	codeChallenge string
	resource      string
	expires       time.Time
}

// oauthCode is an authorization code. Single use: redeeming deletes it, and a
// second presentation finds nothing.
type oauthCode struct {
	clientID      string
	redirectURI   string
	codeChallenge string
	namespace     string
	resource      string
	expires       time.Time
}

// SetMCPOAuth turns on OAuth for the hosted MCP endpoint. seed is the same
// WANCTL_MCP_SEED the MCP handler runs on — sharing it is what lets the MCP
// side verify an access token with no lookup here.
func (r *Relay) SetMCPOAuth(seed []byte, store OAuthStore) {
	r.mcpSeed = append([]byte(nil), seed...)
	r.oauthStore = store
	r.oauthRequests = map[string]*oauthAuthzRequest{}
	r.oauthCodes = map[string]*oauthCode{}
}

func (r *Relay) oauthEnabled() bool {
	return len(r.mcpSeed) > 0 && r.oauthStore != nil && r.admin != nil && publicOrigin() != ""
}

// publicOrigin is the relay's own https origin, from configuration rather than
// from the request. Everything below is an identifier a client compares
// byte-for-byte across three different endpoints, so a Host header deciding it
// would let one poisoned request rewrite the issuer.
func publicOrigin() string {
	origin := strings.TrimRight(os.Getenv("WANCTL_PUBLIC_ORIGIN"), "/")
	if origin == "" {
		return ""
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	return origin
}

func portalOrigin() string {
	return strings.TrimRight(os.Getenv("WANCTL_PORTAL"), "/")
}

func (r *Relay) registerOAuth(mux *http.ServeMux) {
	// The well-known paths answer whether or not OAuth is configured: a 404
	// tells a client "this server has no authorization server", which is the
	// truth on a relay that never set a seed. The handlers check inside.
	mux.HandleFunc("/.well-known/oauth-protected-resource", r.oauthProtectedResource)
	mux.HandleFunc("/.well-known/oauth-protected-resource/", r.oauthProtectedResource)
	mux.HandleFunc("/.well-known/oauth-authorization-server", r.oauthAuthorizationServer)
	mux.HandleFunc("/.well-known/oauth-authorization-server/", r.oauthAuthorizationServer)
	mux.HandleFunc("/oauth/register", r.oauthRegister)
	mux.HandleFunc("/oauth/token", r.oauthToken)
	mux.HandleFunc("/oauth/revoke", r.oauthRevoke)
	mux.HandleFunc("/admin/oauth/authorize-request", r.adminOAuthAuthorizeRequest)
	mux.HandleFunc("/admin/oauth/approve", r.adminOAuthApprove)
	mux.HandleFunc("/admin/oauth/deny", r.adminOAuthDeny)
}

// --- discovery ---

// MCPResourceURL is the resource identifier clients bind their tokens to.
func MCPResourceURL() string {
	if origin := publicOrigin(); origin != "" {
		return origin + "/mcp"
	}
	return ""
}

// validResource accepts the canonical resource and the /wanctl-mcp alias, so a
// client that discovered the endpoint through the alias is not turned away for
// echoing back the URL we gave it.
func validResource(resource string) bool {
	origin := publicOrigin()
	if origin == "" {
		return false
	}
	resource = strings.TrimRight(resource, "/")
	return resource == origin+"/mcp" || resource == origin+"/wanctl-mcp"
}

func (r *Relay) oauthProtectedResource(w http.ResponseWriter, req *http.Request) {
	if !r.oauthMetadataOK(w, req) {
		return
	}
	origin := publicOrigin()
	writeOAuthJSON(w, map[string]any{
		"resource":                 origin + "/mcp",
		"authorization_servers":    []string{origin},
		"bearer_methods_supported": []string{"header"},
	})
}

func (r *Relay) oauthAuthorizationServer(w http.ResponseWriter, req *http.Request) {
	if !r.oauthMetadataOK(w, req) {
		return
	}
	origin := publicOrigin()
	writeOAuthJSON(w, map[string]any{
		"issuer":                                origin,
		"authorization_endpoint":                portalOrigin() + "/oauth/authorize",
		"token_endpoint":                        origin + "/oauth/token",
		"registration_endpoint":                 origin + "/oauth/register",
		"revocation_endpoint":                   origin + "/oauth/revoke",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_post", "client_secret_basic"},
		"scopes_supported":                      []string{oauthScope},
	})
}

func (r *Relay) oauthMetadataOK(w http.ResponseWriter, req *http.Request) bool {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if !r.oauthEnabled() || portalOrigin() == "" {
		http.NotFound(w, req)
		return false
	}
	return true
}

func writeOAuthJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	// Discovery documents are read by clients that will cache them; a short
	// cache is fine and keeps a connector's first call from hitting us four
	// times for the same two documents.
	w.Header().Set("Cache-Control", "public, max-age=300")
	json.NewEncoder(w).Encode(v)
}

// --- dynamic client registration (RFC 7591) ---

func (r *Relay) oauthRegister(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !r.oauthEnabled() {
		http.NotFound(w, req)
		return
	}
	var body struct {
		ClientName              string   `json:"client_name"`
		RedirectURIs            []string `json:"redirect_uris"`
		GrantTypes              []string `json:"grant_types"`
		ResponseTypes           []string `json:"response_types"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		Scope                   string   `json:"scope"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "body must be JSON")
		return
	}
	if len(body.RedirectURIs) == 0 || len(body.RedirectURIs) > oauthMaxRedirects {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri",
			fmt.Sprintf("supply 1 to %d redirect_uris", oauthMaxRedirects))
		return
	}
	for _, u := range body.RedirectURIs {
		if err := validRedirectURI(u); err != nil {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", err.Error())
			return
		}
	}
	method := body.TokenEndpointAuthMethod
	if method == "" {
		method = "client_secret_post"
	}
	switch method {
	case "none", "client_secret_post", "client_secret_basic":
	default:
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata",
			"token_endpoint_auth_method must be none, client_secret_post or client_secret_basic")
		return
	}
	name := strings.TrimSpace(body.ClientName)
	if name == "" {
		name = "Unnamed MCP client"
	}
	if len(name) > 120 {
		name = name[:120]
	}
	client := OAuthClient{
		ID:           "wco_" + randHex(16),
		Name:         name,
		RedirectURIs: body.RedirectURIs,
		AuthMethod:   method,
		CreatedAt:    time.Now(),
	}
	secret := ""
	if method != "none" {
		secret = "wcs_" + randHex(32)
		client.SecretHash = HashToken(secret)
	}
	if err := r.oauthStore.PutOAuthClient(client); err != nil {
		http.Error(w, "store client: "+err.Error(), http.StatusInternalServerError)
		return
	}
	out := map[string]any{
		"client_id":                  client.ID,
		"client_id_issued_at":        client.CreatedAt.Unix(),
		"client_name":                client.Name,
		"redirect_uris":              client.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": method,
		"scope":                      oauthScope,
	}
	if secret != "" {
		out["client_secret"] = secret
		out["client_secret_expires_at"] = 0
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(out)
}

// validRedirectURI allows https anywhere and http only on the loopback
// interface, which is the one place a plaintext hop cannot be observed by a
// network. A fragment is refused because the authorization response appends
// its own query and a fragment would swallow it.
func validRedirectURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("redirect_uri %q is not an absolute URL", raw)
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return fmt.Errorf("redirect_uri %q must not carry a fragment", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return nil
		}
		return fmt.Errorf("redirect_uri %q may only use http on loopback", raw)
	default:
		return fmt.Errorf("redirect_uri %q must be https (or http on loopback)", raw)
	}
}

// --- the portal's two internal calls ---

// adminOAuthAuthorizeRequest validates what the browser arrived carrying and
// records it. It deliberately mints nothing: this is reached by a plain GET
// navigation on the portal, and a GET that creates a credential is a GET an
// attacker can cause (audit 2026-08-28, SEC-C-04, which is the same lesson
// /admin/enroll/mint learned).
func (r *Relay) adminOAuthAuthorizeRequest(w http.ResponseWriter, req *http.Request) {
	if !r.adminOK(req) || !r.oauthEnabled() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var body struct {
		ClientID            string `json:"client_id"`
		RedirectURI         string `json:"redirect_uri"`
		ResponseType        string `json:"response_type"`
		CodeChallenge       string `json:"code_challenge"`
		CodeChallengeMethod string `json:"code_challenge_method"`
		State               string `json:"state"`
		Scope               string `json:"scope"`
		Resource            string `json:"resource"`
	}
	json.NewDecoder(req.Body).Decode(&body)

	client, found, err := r.oauthStore.OAuthClient(body.ClientID)
	if err != nil {
		http.Error(w, "lookup client: "+err.Error(), http.StatusBadGateway)
		return
	}
	// Everything below is refused in place rather than redirected. The spec is
	// explicit that an unregistered client or an unregistered redirect_uri must
	// never be bounced back to the URI it asked for — that is the open-redirect
	// the registration step exists to close.
	if !found {
		oauthError(w, http.StatusBadRequest, "invalid_client", "unknown client_id; register first")
		return
	}
	if !clientOwnsRedirect(client, body.RedirectURI) {
		oauthError(w, http.StatusBadRequest, "invalid_request", "redirect_uri is not registered for this client")
		return
	}
	if body.ResponseType != "code" {
		oauthError(w, http.StatusBadRequest, "unsupported_response_type", "only response_type=code is supported")
		return
	}
	if body.CodeChallengeMethod != "S256" || len(body.CodeChallenge) < 43 || len(body.CodeChallenge) > 128 {
		oauthError(w, http.StatusBadRequest, "invalid_request", "PKCE with code_challenge_method=S256 is required")
		return
	}
	if body.Scope != "" && body.Scope != oauthScope {
		oauthError(w, http.StatusBadRequest, "invalid_scope", "the only scope is "+oauthScope)
		return
	}
	// RFC 8707: a client that names the resource must name this one. A client
	// that names none is taken to mean this server, which is the only resource
	// this authorization server serves.
	if body.Resource != "" && !validResource(body.Resource) {
		oauthError(w, http.StatusBadRequest, "invalid_target", "resource must be "+MCPResourceURL())
		return
	}
	ar := &oauthAuthzRequest{
		id:            randHex(16),
		clientID:      client.ID,
		clientName:    client.Name,
		redirectURI:   body.RedirectURI,
		state:         body.State,
		codeChallenge: body.CodeChallenge,
		resource:      body.Resource,
		expires:       time.Now().Add(oauthRequestTTL),
	}
	r.oauthMu.Lock()
	r.purgeOAuthLocked()
	r.oauthRequests[ar.id] = ar
	r.oauthMu.Unlock()
	writeJSON(w, map[string]any{
		"request_id":   ar.id,
		"client_name":  ar.clientName,
		"client_id":    ar.clientID,
		"redirect_uri": ar.redirectURI,
		"expires_in":   int(oauthRequestTTL.Seconds()),
	})
}

// adminOAuthApprove is the only place an authorization code is born, and the
// portal reaches it only from a CSRF-protected POST a signed-in human made.
func (r *Relay) adminOAuthApprove(w http.ResponseWriter, req *http.Request) {
	if !r.adminOK(req) || !r.oauthEnabled() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var body struct {
		RequestID string `json:"request_id"`
		Namespace string `json:"namespace"`
	}
	json.NewDecoder(req.Body).Decode(&body)
	if body.Namespace == "" {
		http.Error(w, "namespace required", http.StatusBadRequest)
		return
	}
	ar, ok := r.takeOAuthRequest(body.RequestID)
	if !ok {
		http.Error(w, "request expired or already answered", http.StatusConflict)
		return
	}
	code := "wac_" + randHex(24)
	r.oauthMu.Lock()
	r.oauthCodes[code] = &oauthCode{
		clientID:      ar.clientID,
		redirectURI:   ar.redirectURI,
		codeChallenge: ar.codeChallenge,
		namespace:     body.Namespace,
		resource:      ar.resource,
		expires:       time.Now().Add(oauthCodeTTL),
	}
	r.oauthMu.Unlock()
	if r.audit != nil {
		r.audit.Audit(body.Namespace, "", "mcp-oauth-approve "+ar.clientID)
	}
	writeJSON(w, map[string]any{"redirect": redirectWith(ar.redirectURI, url.Values{
		"code":  {code},
		"state": {ar.state},
	})})
}

// adminOAuthDeny answers the client the way the spec asks, so a refusal reads
// as a decision rather than as a broken page.
func (r *Relay) adminOAuthDeny(w http.ResponseWriter, req *http.Request) {
	if !r.adminOK(req) || !r.oauthEnabled() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var body struct {
		RequestID string `json:"request_id"`
	}
	json.NewDecoder(req.Body).Decode(&body)
	ar, ok := r.takeOAuthRequest(body.RequestID)
	if !ok {
		http.Error(w, "request expired or already answered", http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{"redirect": redirectWith(ar.redirectURI, url.Values{
		"error":             {"access_denied"},
		"error_description": {"the account owner declined this request"},
		"state":             {ar.state},
	})})
}

func (r *Relay) takeOAuthRequest(id string) (*oauthAuthzRequest, bool) {
	r.oauthMu.Lock()
	defer r.oauthMu.Unlock()
	r.purgeOAuthLocked()
	ar := r.oauthRequests[id]
	if ar == nil {
		return nil, false
	}
	delete(r.oauthRequests, id)
	return ar, true
}

// redirectWith appends the authorization response to the client's URI,
// preserving any query the client already had there. state is omitted when the
// client sent none rather than returned empty, because a client that echoes
// what it gets would then compare "" against absent.
func redirectWith(redirectURI string, params url.Values) string {
	if params.Get("state") == "" {
		params.Del("state")
	}
	u, err := url.Parse(redirectURI)
	if err != nil {
		return redirectURI
	}
	q := u.Query()
	for k, vs := range params {
		for _, v := range vs {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func clientOwnsRedirect(c OAuthClient, uri string) bool {
	for _, registered := range c.RedirectURIs {
		if registered == uri {
			return true
		}
	}
	return false
}

func (r *Relay) purgeOAuthLocked() {
	now := time.Now()
	for id, ar := range r.oauthRequests {
		if now.After(ar.expires) {
			delete(r.oauthRequests, id)
		}
	}
	for code, c := range r.oauthCodes {
		if now.After(c.expires) {
			delete(r.oauthCodes, code)
		}
	}
}

// --- token endpoint ---

func (r *Relay) oauthToken(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !r.oauthEnabled() {
		http.NotFound(w, req)
		return
	}
	if err := req.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "body must be application/x-www-form-urlencoded")
		return
	}
	client, ok := r.authenticateOAuthClient(w, req)
	if !ok {
		return
	}
	switch req.PostForm.Get("grant_type") {
	case "authorization_code":
		r.oauthTokenFromCode(w, req, client)
	case "refresh_token":
		r.oauthTokenFromRefresh(w, req, client)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type",
			"grant_type must be authorization_code or refresh_token")
	}
}

// authenticateOAuthClient accepts the client's id from the form and its secret
// from either the form or HTTP Basic, which are the two methods the metadata
// advertises alongside "none".
func (r *Relay) authenticateOAuthClient(w http.ResponseWriter, req *http.Request) (OAuthClient, bool) {
	id, secret := req.PostForm.Get("client_id"), req.PostForm.Get("client_secret")
	if basicID, basicSecret, has := req.BasicAuth(); has {
		if id != "" && id != basicID {
			oauthError(w, http.StatusUnauthorized, "invalid_client", "client_id in body and Authorization disagree")
			return OAuthClient{}, false
		}
		id, secret = basicID, basicSecret
	}
	if id == "" {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "client_id required")
		return OAuthClient{}, false
	}
	client, found, err := r.oauthStore.OAuthClient(id)
	if err != nil {
		http.Error(w, "lookup client: "+err.Error(), http.StatusBadGateway)
		return OAuthClient{}, false
	}
	if !found {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "unknown client_id")
		return OAuthClient{}, false
	}
	if client.SecretHash == "" {
		return client, true
	}
	if subtle.ConstantTimeCompare([]byte(HashToken(secret)), []byte(client.SecretHash)) != 1 {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
		return OAuthClient{}, false
	}
	return client, true
}

func (r *Relay) oauthTokenFromCode(w http.ResponseWriter, req *http.Request, client OAuthClient) {
	code := req.PostForm.Get("code")
	r.oauthMu.Lock()
	r.purgeOAuthLocked()
	c := r.oauthCodes[code]
	if c != nil {
		// Single use, and consumed before any check can fail: a code that was
		// presented once must not survive to be presented again, whatever the
		// outcome of the checks below.
		delete(r.oauthCodes, code)
	}
	r.oauthMu.Unlock()
	if c == nil {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "authorization code is unknown, expired or already used")
		return
	}
	if c.clientID != client.ID {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "authorization code was issued to another client")
		return
	}
	if got := req.PostForm.Get("redirect_uri"); got != c.redirectURI {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the authorization request")
		return
	}
	if res := req.PostForm.Get("resource"); res != "" && !validResource(res) {
		oauthError(w, http.StatusBadRequest, "invalid_target", "resource must be "+MCPResourceURL())
		return
	}
	if !pkceMatches(c.codeChallenge, req.PostForm.Get("code_verifier")) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "code_verifier does not match code_challenge")
		return
	}
	// The namespace token is minted here and nowhere earlier. A consent the
	// client never redeemed therefore leaves no live credential behind.
	token, err := r.admin.IssueToken(c.namespace, "oauth:"+client.Name, 0)
	if err != nil {
		http.Error(w, "issue token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	r.writeOAuthTokens(w, c.namespace, token, client.ID)
}

func (r *Relay) oauthTokenFromRefresh(w http.ResponseWriter, req *http.Request, client OAuthClient) {
	presented := req.PostForm.Get("refresh_token")
	if presented == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "refresh_token required")
		return
	}
	row, found, err := r.oauthStore.OAuthRefresh(HashToken(presented))
	if err != nil {
		http.Error(w, "lookup refresh token: "+err.Error(), http.StatusBadGateway)
		return
	}
	if !found || !row.RevokedAt.IsZero() || time.Now().After(row.ExpiresAt) || row.ClientID != client.ID {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh token is unknown, expired, revoked or issued to another client")
		return
	}
	claim, err := mcpauth.OpenGrant(r.mcpSeed, row.Grant, time.Now())
	if err != nil {
		// The seed rotated, which is the documented way to log every hosted
		// session out at once. Say so as an auth failure, not a server error.
		oauthError(w, http.StatusBadRequest, "invalid_grant", "this authorization is no longer valid; authorize again")
		return
	}
	// Rotate: the presented token dies here whether or not the client ever
	// receives the replacement, so a stolen refresh token is usable at most
	// once and the theft shows up as the real client being logged out.
	if err := r.oauthStore.RevokeOAuthRefresh(row.Hash); err != nil {
		http.Error(w, "rotate refresh token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	r.writeOAuthTokens(w, claim.Namespace, claim.Token, client.ID)
}

// writeOAuthTokens seals the pair and stores the replacement refresh row.
func (r *Relay) writeOAuthTokens(w http.ResponseWriter, namespace, relayToken, clientID string) {
	now := time.Now()
	access, _, err := mcpauth.SealAccess(r.mcpSeed, namespace, relayToken, clientID, now, oauthAccessTTL)
	if err != nil {
		http.Error(w, "seal access token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	grant, _, err := mcpauth.SealGrant(r.mcpSeed, namespace, relayToken, clientID, now, oauthRefreshTTL)
	if err != nil {
		http.Error(w, "seal grant: "+err.Error(), http.StatusInternalServerError)
		return
	}
	refresh := "wrt_" + randHex(32)
	if err := r.oauthStore.PutOAuthRefresh(OAuthRefresh{
		Hash:      HashToken(refresh),
		ClientID:  clientID,
		Namespace: namespace,
		Grant:     grant,
		ExpiresAt: now.Add(oauthRefreshTTL),
	}); err != nil {
		http.Error(w, "store refresh token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(oauthAccessTTL.Seconds()),
		"refresh_token": refresh,
		"scope":         oauthScope,
	})
}

// --- revocation (RFC 7009) ---

func (r *Relay) oauthRevoke(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !r.oauthEnabled() {
		http.NotFound(w, req)
		return
	}
	if err := req.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "body must be application/x-www-form-urlencoded")
		return
	}
	client, ok := r.authenticateOAuthClient(w, req)
	if !ok {
		return
	}
	// RFC 7009 wants 200 for an unknown token too: the client's goal is "this
	// token no longer works", and it already does not.
	if token := req.PostForm.Get("token"); token != "" {
		if row, found, err := r.oauthStore.OAuthRefresh(HashToken(token)); err == nil && found && row.ClientID == client.ID {
			r.revokeOAuthGrant(row)
		}
	}
	w.WriteHeader(http.StatusOK)
}

// revokeOAuthGrant takes down both halves: the refresh row, and the namespace
// token the grant is standing on — otherwise a still-live access token would
// keep working for up to an hour after the user revoked it.
func (r *Relay) revokeOAuthGrant(row OAuthRefresh) {
	_ = r.oauthStore.RevokeOAuthRefresh(row.Hash)
	if claim, err := mcpauth.OpenGrant(r.mcpSeed, row.Grant, time.Now()); err == nil {
		_ = r.oauthStore.RevokeRelayTokenHash(claim.Namespace, HashToken(claim.Token))
	}
}

// RevokeOAuthRelayToken revokes the namespace token behind a live MCP OAuth
// session. It is what wanctl_logout calls on the OAuth path, where "log out"
// has to mean something durable: the bearer is stateless, so the only way to
// stop it is to stop the token it carries.
func (r *Relay) RevokeOAuthRelayToken(namespace, token string) error {
	if r.oauthStore == nil {
		return fmt.Errorf("oauth is not configured")
	}
	return r.oauthStore.RevokeRelayTokenHash(namespace, HashToken(token))
}

// ResolveOAuthToken reports whether a namespace token carried by an access
// token is still live. The MCP endpoint asks this on every bearer request, so
// a revoked grant stops working within one call rather than at the next hour.
func (r *Relay) ResolveOAuthToken(namespace, token string) bool {
	ns, ok := r.ts.Resolve(token)
	return ok && ns == namespace
}

// pkceMatches is the S256 check: the verifier the client kept private must
// hash to the challenge it published when it started the trip. This is what
// stops a stolen authorization code being redeemed by whoever stole it.
func pkceMatches(challenge, verifier string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

func oauthError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="wanctl"`)
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{
		"error":             code,
		"error_description": description,
	})
}
