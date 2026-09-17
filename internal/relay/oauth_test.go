package relay

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"wanctl/internal/mcpauth"
)

// --- fixtures ---

const oauthTestSeed = "0123456789abcdef0123456789abcdef0123456789abcdef"

// oauthBackend is the token store, the admin store and the OAuth store at once,
// because in production they are one Postgres and the interesting assertions
// are about what the three of them agree on: a token this flow issued must be
// resolvable, and revoking it must stop it resolving.
type oauthBackend struct {
	noopAdmin
	mu       sync.Mutex
	next     int
	tokens   map[string]string // raw token -> namespace
	revoked  map[string]bool   // token hash -> revoked
	labels   map[string]string // raw token -> label
	clients  map[string]OAuthClient
	refresh  map[string]OAuthRefresh
	issuedNS []string
}

func newOAuthBackend() *oauthBackend {
	return &oauthBackend{
		tokens: map[string]string{}, revoked: map[string]bool{}, labels: map[string]string{},
		clients: map[string]OAuthClient{}, refresh: map[string]OAuthRefresh{},
	}
}

func (b *oauthBackend) Resolve(token string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ns, ok := b.tokens[token]
	if !ok || b.revoked[HashToken(token)] {
		return "", false
	}
	return ns, true
}

func (b *oauthBackend) IssueToken(namespace, label string, _ int) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	raw := "wanctl_test_" + namespace + "_" + string(rune('a'+b.next))
	b.tokens[raw] = namespace
	b.labels[raw] = label
	b.issuedNS = append(b.issuedNS, namespace)
	return raw, nil
}

func (b *oauthBackend) PutOAuthClient(c OAuthClient) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clients[c.ID] = c
	return nil
}

func (b *oauthBackend) OAuthClient(id string) (OAuthClient, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.clients[id]
	return c, ok, nil
}

func (b *oauthBackend) PutOAuthRefresh(t OAuthRefresh) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refresh[t.Hash] = t
	return nil
}

func (b *oauthBackend) OAuthRefresh(hash string) (OAuthRefresh, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.refresh[hash]
	return t, ok, nil
}

func (b *oauthBackend) RevokeOAuthRefresh(hash string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.refresh[hash]
	if ok {
		t.RevokedAt = time.Now()
		b.refresh[hash] = t
	}
	return nil
}

func (b *oauthBackend) RevokeRelayTokenHash(_, hash string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.revoked[hash] = true
	return nil
}

func (b *oauthBackend) label(token string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.labels[token]
}

func newOAuthRelay(t *testing.T) (*Relay, *oauthBackend, []byte) {
	t.Helper()
	t.Setenv("WANCTL_PUBLIC_ORIGIN", "https://relay.example")
	t.Setenv("WANCTL_PORTAL", "https://portal.example")
	b := newOAuthBackend()
	r := New(b)
	r.SetAdmin(b)
	r.SetAdminSecret(strings.Repeat("s", 32))
	seed := []byte(oauthTestSeed)
	r.SetMCPOAuth(seed, b)
	return r, b, seed
}

func oauthDo(t *testing.T, r *Relay, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	return rr
}

func adminPost(t *testing.T, r *Relay, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(b)))
	req.Header.Set("X-Admin-Secret", strings.Repeat("s", 32))
	req.Header.Set("Content-Type", "application/json")
	return oauthDo(t, r, req)
}

func formPost(t *testing.T, r *Relay, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return oauthDo(t, r, req)
}

func decode(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("body %q is not JSON: %v", rr.Body.String(), err)
	}
	return out
}

const testRedirect = "https://chatgpt.com/connector_platform_oauth_redirect"

func registerClient(t *testing.T, r *Relay) (id, secret string) {
	t.Helper()
	rr := adminRegister(t, r, map[string]any{
		"client_name":                "ChatGPT",
		"redirect_uris":              []string{testRedirect},
		"token_endpoint_auth_method": "client_secret_post",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rr.Code, rr.Body.String())
	}
	out := decode(t, rr)
	id, _ = out["client_id"].(string)
	secret, _ = out["client_secret"].(string)
	if id == "" || secret == "" {
		t.Fatalf("register returned %v", out)
	}
	return id, secret
}

func adminRegister(t *testing.T, r *Relay, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	return oauthDo(t, r, req)
}

// pkce returns a verifier and its S256 challenge.
func pkce() (verifier, challenge string) {
	verifier = strings.Repeat("v", 64)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// authorize walks the browser half: the portal asks the relay to validate the
// request, then a signed-in human approves it. Returns the authorization code.
func authorize(t *testing.T, r *Relay, clientID, challenge, state, namespace string) string {
	t.Helper()
	rr := adminPost(t, r, "/admin/oauth/authorize-request", map[string]string{
		"client_id": clientID, "redirect_uri": testRedirect, "response_type": "code",
		"code_challenge": challenge, "code_challenge_method": "S256",
		"state": state, "scope": "wanctl", "resource": "https://relay.example/mcp",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("authorize-request: %d %s", rr.Code, rr.Body.String())
	}
	requestID, _ := decode(t, rr)["request_id"].(string)
	rr = adminPost(t, r, "/admin/oauth/approve", map[string]string{
		"request_id": requestID, "namespace": namespace,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rr.Code, rr.Body.String())
	}
	redirect, _ := decode(t, rr)["redirect"].(string)
	u, err := url.Parse(redirect)
	if err != nil {
		t.Fatalf("approve returned an unparsable redirect %q: %v", redirect, err)
	}
	if got := u.Query().Get("state"); got != state {
		t.Fatalf("redirect state = %q, want %q", got, state)
	}
	code := u.Query().Get("code")
	if code == "" {
		t.Fatalf("redirect carries no code: %s", redirect)
	}
	return code
}

// --- discovery ---

func TestProtectedResourceMetadataIsServedAtBothPaths(t *testing.T) {
	r, _, _ := newOAuthRelay(t)
	// ChatGPT probes the bare path; the spec's path-aware form appends the
	// resource's path. Both have to answer or discovery stops at the first 404.
	for _, path := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/mcp",
		"/.well-known/oauth-protected-resource/wanctl-mcp",
	} {
		rr := oauthDo(t, r, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
		}
		out := decode(t, rr)
		if out["resource"] != "https://relay.example/mcp" {
			t.Errorf("%s: resource = %v", path, out["resource"])
		}
		servers, _ := out["authorization_servers"].([]any)
		if len(servers) != 1 || servers[0] != "https://relay.example" {
			t.Errorf("%s: authorization_servers = %v", path, out["authorization_servers"])
		}
	}
}

func TestAuthorizationServerMetadataPointsAtPortalAndRelay(t *testing.T) {
	r, _, _ := newOAuthRelay(t)
	rr := oauthDo(t, r, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	out := decode(t, rr)
	want := map[string]string{
		"issuer":                 "https://relay.example",
		"authorization_endpoint": "https://portal.example/oauth/authorize",
		"token_endpoint":         "https://relay.example/oauth/token",
		"registration_endpoint":  "https://relay.example/oauth/register",
		"revocation_endpoint":    "https://relay.example/oauth/revoke",
	}
	for k, v := range want {
		if out[k] != v {
			t.Errorf("%s = %v, want %q", k, out[k], v)
		}
	}
	methods, _ := out["code_challenge_methods_supported"].([]any)
	if len(methods) != 1 || methods[0] != "S256" {
		t.Errorf("code_challenge_methods_supported = %v", out["code_challenge_methods_supported"])
	}
	auth, _ := out["token_endpoint_auth_methods_supported"].([]any)
	if len(auth) != 3 {
		t.Errorf("token_endpoint_auth_methods_supported = %v", out["token_endpoint_auth_methods_supported"])
	}
}

// A relay with no seed, no store or no public origin has no authorization
// server, and saying so is how a client learns to stop looking.
func TestDiscoveryIsAbsentWhenOAuthIsOff(t *testing.T) {
	t.Setenv("WANCTL_PUBLIC_ORIGIN", "https://relay.example")
	t.Setenv("WANCTL_PORTAL", "https://portal.example")
	r := New(envTokens{})
	for _, path := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-authorization-server",
	} {
		rr := oauthDo(t, r, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, rr.Code)
		}
	}
}

// --- dynamic client registration ---

func TestDynamicRegistrationIssuesCredentials(t *testing.T) {
	r, b, _ := newOAuthRelay(t)
	id, secret := registerClient(t, r)
	client, ok, _ := b.OAuthClient(id)
	if !ok {
		t.Fatal("client was not stored")
	}
	if client.SecretHash != HashToken(secret) {
		t.Error("stored secret hash does not match the secret handed to the client")
	}
	if client.Name != "ChatGPT" || len(client.RedirectURIs) != 1 {
		t.Errorf("stored client = %+v", client)
	}
}

func TestRegistrationRefusesUnsafeRedirects(t *testing.T) {
	r, _, _ := newOAuthRelay(t)
	for _, uri := range []string{
		"http://chatgpt.com/cb",       // plaintext off-loopback
		"https://chatgpt.com/cb#frag", // a fragment would swallow our query
		"chatgpt://cb",                // custom scheme
		"/relative",                   // not absolute
	} {
		rr := adminRegister(t, r, map[string]any{"client_name": "x", "redirect_uris": []string{uri}})
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400 (%s)", uri, rr.Code, rr.Body.String())
		}
	}
	// Loopback http is how a desktop client receives its callback.
	rr := adminRegister(t, r, map[string]any{"client_name": "x", "redirect_uris": []string{"http://127.0.0.1:7777/cb"}})
	if rr.Code != http.StatusCreated {
		t.Errorf("loopback redirect refused: %d %s", rr.Code, rr.Body.String())
	}
}

func TestRegistrationWithoutClientAuthGetsNoSecret(t *testing.T) {
	r, _, _ := newOAuthRelay(t)
	rr := adminRegister(t, r, map[string]any{
		"client_name": "public", "redirect_uris": []string{testRedirect},
		"token_endpoint_auth_method": "none",
	})
	out := decode(t, rr)
	if _, has := out["client_secret"]; has {
		t.Error("a client registered with auth method none was given a secret")
	}
}

// --- the authorization code leg ---

func TestAuthorizationCodeFlowMintsAWorkingBearer(t *testing.T) {
	r, b, seed := newOAuthRelay(t)
	id, secret := registerClient(t, r)
	verifier, challenge := pkce()
	code := authorize(t, r, id, challenge, "st-1", "alice")

	rr := formPost(t, r, "/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {testRedirect},
		"code_verifier": {verifier},
		"client_id":     {id},
		"client_secret": {secret},
		"resource":      {"https://relay.example/mcp"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("token: %d %s", rr.Code, rr.Body.String())
	}
	out := decode(t, rr)
	if out["token_type"] != "Bearer" || out["scope"] != "wanctl" {
		t.Errorf("token response = %v", out)
	}
	access, _ := out["access_token"].(string)
	claim, err := mcpauth.OpenAccess(seed, access, time.Now())
	if err != nil {
		t.Fatalf("access token does not open with the MCP seed: %v", err)
	}
	if claim.Namespace != "alice" || claim.ClientID != id {
		t.Errorf("claim = %+v", claim)
	}
	if ns, ok := b.Resolve(claim.Token); !ok || ns != "alice" {
		t.Errorf("the relay token inside the bearer does not resolve: ns=%q ok=%v", ns, ok)
	}
	if got := b.label(claim.Token); got != "oauth:ChatGPT" {
		t.Errorf("relay token label = %q, want oauth:ChatGPT", got)
	}
	if _, ok := out["refresh_token"].(string); !ok {
		t.Error("no refresh token was issued")
	}
}

func TestAuthorizationCodeRejectsAWrongVerifier(t *testing.T) {
	r, b, _ := newOAuthRelay(t)
	id, secret := registerClient(t, r)
	_, challenge := pkce()
	code := authorize(t, r, id, challenge, "st", "alice")

	rr := formPost(t, r, "/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {testRedirect},
		"code_verifier": {strings.Repeat("w", 64)}, "client_id": {id}, "client_secret": {secret},
	})
	if rr.Code != http.StatusBadRequest || decode(t, rr)["error"] != "invalid_grant" {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	if len(b.issuedNS) != 0 {
		t.Errorf("a failed exchange still minted namespace tokens: %v", b.issuedNS)
	}
}

func TestAuthorizationCodeRejectsAChangedRedirectOrResource(t *testing.T) {
	r, _, _ := newOAuthRelay(t)
	id, secret := registerClient(t, r)
	verifier, challenge := pkce()

	for name, form := range map[string]url.Values{
		"redirect_uri": {"redirect_uri": {"https://chatgpt.com/elsewhere"}},
		"resource":     {"redirect_uri": {testRedirect}, "resource": {"https://relay.example/other"}},
	} {
		code := authorize(t, r, id, challenge, "st", "alice")
		values := url.Values{
			"grant_type": {"authorization_code"}, "code": {code},
			"code_verifier": {verifier}, "client_id": {id}, "client_secret": {secret},
		}
		for k, v := range form {
			values[k] = v
		}
		rr := formPost(t, r, "/oauth/token", values)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%s)", name, rr.Code, rr.Body.String())
		}
	}
}

// A code is a one-time credential. The second presentation must fail even
// though the first succeeded, or a code left in a log or a Referer is reusable.
func TestAuthorizationCodeIsSingleUse(t *testing.T) {
	r, _, _ := newOAuthRelay(t)
	id, secret := registerClient(t, r)
	verifier, challenge := pkce()
	code := authorize(t, r, id, challenge, "st", "alice")

	exchange := func() *httptest.ResponseRecorder {
		return formPost(t, r, "/oauth/token", url.Values{
			"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {testRedirect},
			"code_verifier": {verifier}, "client_id": {id}, "client_secret": {secret},
		})
	}
	if rr := exchange(); rr.Code != http.StatusOK {
		t.Fatalf("first exchange: %d %s", rr.Code, rr.Body.String())
	}
	rr := exchange()
	if rr.Code != http.StatusBadRequest || decode(t, rr)["error"] != "invalid_grant" {
		t.Fatalf("second exchange: %d %s", rr.Code, rr.Body.String())
	}
}

func TestTokenEndpointChecksClientCredentials(t *testing.T) {
	r, _, _ := newOAuthRelay(t)
	id, _ := registerClient(t, r)
	verifier, challenge := pkce()
	code := authorize(t, r, id, challenge, "st", "alice")

	rr := formPost(t, r, "/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {testRedirect},
		"code_verifier": {verifier}, "client_id": {id}, "client_secret": {"wrong"},
	})
	if rr.Code != http.StatusUnauthorized || decode(t, rr)["error"] != "invalid_client" {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
}

// HTTP Basic is the other method the metadata advertises, so it has to work.
func TestTokenEndpointAcceptsBasicClientAuth(t *testing.T) {
	r, _, _ := newOAuthRelay(t)
	id, secret := registerClient(t, r)
	verifier, challenge := pkce()
	code := authorize(t, r, id, challenge, "st", "alice")

	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {testRedirect},
		"code_verifier": {verifier},
	}
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(id, secret)
	if rr := oauthDo(t, r, req); rr.Code != http.StatusOK {
		t.Fatalf("basic auth exchange: %d %s", rr.Code, rr.Body.String())
	}
}

// --- what the portal is not allowed to be tricked into ---

func TestAuthorizeRequestRefusesUnregisteredRedirects(t *testing.T) {
	r, _, _ := newOAuthRelay(t)
	id, _ := registerClient(t, r)
	_, challenge := pkce()
	rr := adminPost(t, r, "/admin/oauth/authorize-request", map[string]string{
		"client_id": id, "redirect_uri": "https://evil.example/cb", "response_type": "code",
		"code_challenge": challenge, "code_challenge_method": "S256",
	})
	// Refused here, not redirected: bouncing an unregistered URI back would be
	// the open redirect registration exists to prevent.
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
}

func TestAuthorizeRequestRequiresPKCE(t *testing.T) {
	r, _, _ := newOAuthRelay(t)
	id, _ := registerClient(t, r)
	for _, body := range []map[string]string{
		{"client_id": id, "redirect_uri": testRedirect, "response_type": "code"},
		{"client_id": id, "redirect_uri": testRedirect, "response_type": "code",
			"code_challenge": "short", "code_challenge_method": "S256"},
		{"client_id": id, "redirect_uri": testRedirect, "response_type": "code",
			"code_challenge": strings.Repeat("c", 43), "code_challenge_method": "plain"},
	} {
		if rr := adminPost(t, r, "/admin/oauth/authorize-request", body); rr.Code != http.StatusBadRequest {
			t.Errorf("%v: status %d, want 400", body, rr.Code)
		}
	}
}

func TestAuthorizeRequestRefusesAForeignResource(t *testing.T) {
	r, _, _ := newOAuthRelay(t)
	id, _ := registerClient(t, r)
	_, challenge := pkce()
	rr := adminPost(t, r, "/admin/oauth/authorize-request", map[string]string{
		"client_id": id, "redirect_uri": testRedirect, "response_type": "code",
		"code_challenge": challenge, "code_challenge_method": "S256",
		"resource": "https://someone-else.example/mcp",
	})
	if rr.Code != http.StatusBadRequest || decode(t, rr)["error"] != "invalid_target" {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
}

func TestDenyRedirectsWithAccessDenied(t *testing.T) {
	r, b, _ := newOAuthRelay(t)
	id, _ := registerClient(t, r)
	_, challenge := pkce()
	rr := adminPost(t, r, "/admin/oauth/authorize-request", map[string]string{
		"client_id": id, "redirect_uri": testRedirect, "response_type": "code",
		"code_challenge": challenge, "code_challenge_method": "S256", "state": "st-9",
	})
	requestID, _ := decode(t, rr)["request_id"].(string)
	rr = adminPost(t, r, "/admin/oauth/deny", map[string]string{"request_id": requestID})
	if rr.Code != http.StatusOK {
		t.Fatalf("deny: %d %s", rr.Code, rr.Body.String())
	}
	redirect, _ := decode(t, rr)["redirect"].(string)
	u, _ := url.Parse(redirect)
	if u.Query().Get("error") != "access_denied" || u.Query().Get("state") != "st-9" {
		t.Errorf("deny redirect = %s", redirect)
	}
	if len(b.issuedNS) != 0 {
		t.Errorf("a denied request still minted a token: %v", b.issuedNS)
	}
}

func TestApproveRequiresTheAdminSecret(t *testing.T) {
	r, _, _ := newOAuthRelay(t)
	for _, path := range []string{
		"/admin/oauth/authorize-request", "/admin/oauth/approve", "/admin/oauth/deny",
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		if rr := oauthDo(t, r, req); rr.Code != http.StatusForbidden {
			t.Errorf("%s without the secret: %d, want 403", path, rr.Code)
		}
	}
}

// --- refresh and revocation ---

func TestRefreshRotatesAndRetiresTheOldToken(t *testing.T) {
	r, _, seed := newOAuthRelay(t)
	id, secret := registerClient(t, r)
	verifier, challenge := pkce()
	code := authorize(t, r, id, challenge, "st", "alice")

	first := decode(t, formPost(t, r, "/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {testRedirect},
		"code_verifier": {verifier}, "client_id": {id}, "client_secret": {secret},
	}))
	oldRefresh, _ := first["refresh_token"].(string)

	rr := formPost(t, r, "/oauth/token", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {oldRefresh},
		"client_id": {id}, "client_secret": {secret},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", rr.Code, rr.Body.String())
	}
	second := decode(t, rr)
	newRefresh, _ := second["refresh_token"].(string)
	if newRefresh == "" || newRefresh == oldRefresh {
		t.Fatalf("refresh token was not rotated: %q -> %q", oldRefresh, newRefresh)
	}
	// The new bearer stands on the same namespace token, so refreshing does not
	// litter the user's token list with a new row every hour.
	firstClaim, _ := mcpauth.OpenAccess(seed, first["access_token"].(string), time.Now())
	secondClaim, err := mcpauth.OpenAccess(seed, second["access_token"].(string), time.Now())
	if err != nil {
		t.Fatalf("refreshed access token does not open: %v", err)
	}
	if secondClaim.Token != firstClaim.Token || secondClaim.Namespace != "alice" {
		t.Errorf("refreshed claim = %+v, want the same relay token as %+v", secondClaim, firstClaim)
	}

	replay := formPost(t, r, "/oauth/token", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {oldRefresh},
		"client_id": {id}, "client_secret": {secret},
	})
	if replay.Code != http.StatusBadRequest || decode(t, replay)["error"] != "invalid_grant" {
		t.Fatalf("replaying the retired refresh token: %d %s", replay.Code, replay.Body.String())
	}
}

func TestRefreshRefusesAnotherClientsToken(t *testing.T) {
	r, _, _ := newOAuthRelay(t)
	id, secret := registerClient(t, r)
	otherID, otherSecret := registerClient(t, r)
	verifier, challenge := pkce()
	code := authorize(t, r, id, challenge, "st", "alice")
	first := decode(t, formPost(t, r, "/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {testRedirect},
		"code_verifier": {verifier}, "client_id": {id}, "client_secret": {secret},
	}))
	rr := formPost(t, r, "/oauth/token", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {first["refresh_token"].(string)},
		"client_id": {otherID}, "client_secret": {otherSecret},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
}

func TestRevokeStopsTheNamespaceTokenToo(t *testing.T) {
	r, b, seed := newOAuthRelay(t)
	id, secret := registerClient(t, r)
	verifier, challenge := pkce()
	code := authorize(t, r, id, challenge, "st", "alice")
	out := decode(t, formPost(t, r, "/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {testRedirect},
		"code_verifier": {verifier}, "client_id": {id}, "client_secret": {secret},
	}))
	claim, _ := mcpauth.OpenAccess(seed, out["access_token"].(string), time.Now())

	rr := formPost(t, r, "/oauth/revoke", url.Values{
		"token": {out["refresh_token"].(string)}, "client_id": {id}, "client_secret": {secret},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", rr.Code, rr.Body.String())
	}
	// Revoking only the refresh token would leave the outstanding bearer working
	// for up to an hour — long enough for the user to think it did not work.
	if _, ok := b.Resolve(claim.Token); ok {
		t.Error("the namespace token behind the grant still resolves after revocation")
	}
	if r.ResolveOAuthToken("alice", claim.Token) {
		t.Error("ResolveOAuthToken still accepts the revoked grant")
	}
}

// wanctl_logout on the OAuth path reaches this, and it has to be durable: the
// bearer is stateless, so forgetting it in memory would change nothing.
func TestRevokeOAuthRelayTokenIsWhatLogoutCalls(t *testing.T) {
	r, b, _ := newOAuthRelay(t)
	token, _ := b.IssueToken("alice", "oauth:test", 0)
	if err := r.RevokeOAuthRelayToken("alice", token); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, ok := b.Resolve(token); ok {
		t.Error("token still resolves after logout")
	}
}

// A refresh token whose grant no longer opens — the deployment rotated
// WANCTL_MCP_SEED — is an authorization failure, not a 500.
func TestRefreshAfterASeedRotationFailsAsAuth(t *testing.T) {
	r, b, _ := newOAuthRelay(t)
	id, secret := registerClient(t, r)
	refresh := "wrt_stale"
	b.PutOAuthRefresh(OAuthRefresh{
		Hash: HashToken(refresh), ClientID: id, Namespace: "alice",
		Grant: mcpauth.GrantPrefix + "not-openable", ExpiresAt: time.Now().Add(time.Hour),
	})
	rr := formPost(t, r, "/oauth/token", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {refresh},
		"client_id": {id}, "client_secret": {secret},
	})
	if rr.Code != http.StatusBadRequest || decode(t, rr)["error"] != "invalid_grant" {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
}
