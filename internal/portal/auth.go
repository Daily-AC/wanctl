package portal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GitHub OAuth for self-hosted deployments. The portal historically trusted an
// SSO-injected identity header behind a corporate proxy; a public deployment
// has no such proxy, so this file gives the portal its own login: the standard
// authorization-code flow against github.com (or a GitHub Enterprise host),
// with the signed-in identity carried in an HMAC-signed cookie. Header mode
// remains available and the two are mutually exclusive — when OAuth is
// configured, request headers are never consulted for identity, so a client
// cannot smuggle one past a misconfigured proxy.

const (
	sessionCookieName = "wanctl_session"
	stateCookieName   = "wanctl_oauth_state"
	sessionTTL        = 14 * 24 * time.Hour
	stateTTL          = 10 * time.Minute
	providerGitHub    = "github"
	providerHeader    = "header"
	// pendingInviteBody is the exact 403 body the relay's resolve endpoint
	// returns when admission requires an invite. Contract with internal/relay.
	pendingInviteBody = "pending-invite"
)

// principal is an authenticated identity before namespace resolution.
type principal struct {
	Provider string `json:"provider"`
	Subject  string `json:"sub"`   // header value, or the GitHub numeric id
	Login    string `json:"login"` // display identity; namespace is derived from it
	Name     string `json:"name"`
	Expires  int64  `json:"exp"`
	Issued   int64  `json:"iat"`
}

func (s *Server) oauthEnabled() bool { return s.ghClientID != "" }

// principal returns the authenticated identity for this request, or nil. In
// OAuth mode only the session cookie counts; in header mode only the header.
func (s *Server) principalFrom(r *http.Request) *principal {
	if s.oauthEnabled() {
		p, err := s.decodeSession(cookieValue(r, sessionCookieName))
		if err != nil {
			return nil
		}
		return p
	}
	id := strings.TrimSpace(r.Header.Get(s.userHeader))
	if id == "" {
		return nil
	}
	return &principal{Provider: providerHeader, Subject: id, Login: id}
}

// --- signed session cookie ---

func (s *Server) sign(payload []byte) []byte {
	mac := hmac.New(sha256.New, s.sessionKey)
	mac.Write(payload)
	return mac.Sum(nil)
}

func (s *Server) encodeSession(p *principal) (string, error) {
	if len(s.sessionKey) == 0 {
		return "", errors.New("session secret not configured")
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	b64 := base64.RawURLEncoding
	return "v1." + b64.EncodeToString(raw) + "." + b64.EncodeToString(s.sign(raw)), nil
}

func (s *Server) decodeSession(value string) (*principal, error) {
	if len(s.sessionKey) == 0 || value == "" {
		return nil, errors.New("no session")
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return nil, errors.New("malformed session")
	}
	b64 := base64.RawURLEncoding
	raw, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("malformed session payload")
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("malformed session signature")
	}
	if !hmac.Equal(sig, s.sign(raw)) {
		return nil, errors.New("session signature mismatch")
	}
	var p principal
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if p.Expires <= time.Now().Unix() {
		return nil, errors.New("session expired")
	}
	if p.Provider != providerGitHub || p.Subject == "" || p.Login == "" {
		return nil, errors.New("session claims incomplete")
	}
	return &p, nil
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: value, Path: "/", MaxAge: maxAge,
		Secure: s.cookieSecure(r), HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

// cookieSecure mirrors the CSRF cookie's rule: secure everywhere except plain
// HTTP to a loopback host (local development).
func (s *Server) cookieSecure(r *http.Request) bool {
	if s.publicOrigin != "" || r.TLS != nil || r.URL.Scheme == "https" {
		return true
	}
	host := hostOnly(r.Host)
	return host != "localhost" && host != "127.0.0.1" && host != "::1"
}

// --- OAuth handlers ---

type oauthState struct {
	Nonce   string `json:"nonce"`
	Next    string `json:"next"`
	Expires int64  `json:"exp"`
}

// safeNext accepts only a local absolute path, so the login flow cannot be
// turned into an open redirect.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	// Any control character (including TAB, which a browser strips before
	// resolving "/\t/evil.example" as protocol-relative) makes this unsafe
	// as a redirect target (audit 2026-08-28, SEC-C-05).
	for _, c := range next {
		if c < 0x20 || c == 0x7f || c == '\\' {
			return "/"
		}
	}
	return next
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if !s.oauthEnabled() {
		http.NotFound(w, r)
		return
	}
	nonce := newCSRFToken()
	st := oauthState{Nonce: nonce, Next: safeNext(r.URL.Query().Get("next")), Expires: time.Now().Add(stateTTL).Unix()}
	raw, _ := json.Marshal(st)
	b64 := base64.RawURLEncoding
	// The state cookie must survive the cross-site top-level redirect back from
	// GitHub, which SameSite=Strict would block — Lax is load-bearing here.
	http.SetCookie(w, &http.Cookie{
		Name: stateCookieName, Value: b64.EncodeToString(raw) + "." + b64.EncodeToString(s.sign(raw)),
		Path: "/", MaxAge: int(stateTTL.Seconds()),
		Secure: s.cookieSecure(r), HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	q := url.Values{}
	q.Set("client_id", s.ghClientID)
	q.Set("redirect_uri", s.requestOrigin(r)+"/auth/callback")
	q.Set("state", nonce)
	http.Redirect(w, r, s.ghAuthBase+"/login/oauth/authorize?"+q.Encode(), http.StatusFound)
}

func (s *Server) readStateCookie(r *http.Request) (*oauthState, error) {
	parts := strings.Split(cookieValue(r, stateCookieName), ".")
	if len(parts) != 2 {
		return nil, errors.New("missing state cookie")
	}
	b64 := base64.RawURLEncoding
	raw, err := b64.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("malformed state cookie")
	}
	sig, err := b64.DecodeString(parts[1])
	if err != nil || !hmac.Equal(sig, s.sign(raw)) {
		return nil, errors.New("state cookie signature mismatch")
	}
	var st oauthState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	if st.Expires <= time.Now().Unix() {
		return nil, errors.New("state expired")
	}
	return &st, nil
}

func (s *Server) handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	if !s.oauthEnabled() {
		http.NotFound(w, r)
		return
	}
	st, err := s.readStateCookie(r)
	// Single-use either way: a replayed callback must not find a live state.
	http.SetCookie(w, &http.Cookie{Name: stateCookieName, Value: "", Path: "/", MaxAge: -1})
	if err != nil {
		http.Error(w, "login state invalid; start again at /auth/login", http.StatusBadRequest)
		return
	}
	if got := r.URL.Query().Get("state"); got == "" || !hmac.Equal([]byte(got), []byte(st.Nonce)) {
		http.Error(w, "login state mismatch; start again at /auth/login", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "GitHub declined the login: "+r.URL.Query().Get("error"), http.StatusBadRequest)
		return
	}
	gh, err := s.githubUserForCode(r, code)
	if err != nil {
		http.Error(w, "GitHub login failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	now := time.Now()
	p := &principal{
		Provider: providerGitHub, Subject: gh.subject, Login: gh.login, Name: gh.name,
		Issued: now.Unix(), Expires: now.Add(sessionTTL).Unix(),
	}
	sess, err := s.encodeSession(p)
	if err != nil {
		http.Error(w, "session error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.setSessionCookie(w, r, sess, int(sessionTTL.Seconds()))
	// Resolve once now so a not-yet-invited user lands on the pending page
	// instead of a wall of failing API calls.
	if _, _, status, _ := s.resolveNamespace(p, ""); status == resolvePending {
		http.Redirect(w, r, "/pending", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, st.Next, http.StatusSeeOther)
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	s.setSessionCookie(w, r, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

type githubUser struct {
	subject, login, name string
}

func (s *Server) githubUserForCode(r *http.Request, code string) (*githubUser, error) {
	form := url.Values{}
	form.Set("client_id", s.ghClientID)
	form.Set("client_secret", s.ghClientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", s.requestOrigin(r)+"/auth/callback")
	req, _ := http.NewRequestWithContext(r.Context(), "POST",
		s.ghAuthBase+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	if tok.AccessToken == "" {
		if tok.Error == "" {
			tok.Error = "no access token in response"
		}
		return nil, errors.New("token exchange: " + tok.Error)
	}
	req, _ = http.NewRequestWithContext(r.Context(), "GET", s.ghAPIBase+"/user", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err = s.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /user: status %d", resp.StatusCode)
	}
	var u struct {
		ID    json.Number `json:"id"`
		Login string      `json:"login"`
		Name  string      `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&u); err != nil {
		return nil, err
	}
	if u.ID.String() == "" || u.Login == "" {
		return nil, errors.New("GitHub user response missing id/login")
	}
	return &githubUser{subject: u.ID.String(), login: u.Login, name: u.Name}, nil
}

// requestOrigin is the externally visible origin for redirect URIs. An
// explicit PORTAL_PUBLIC_ORIGIN wins; otherwise derive from the request.
func (s *Server) requestOrigin(r *http.Request) string {
	if s.publicOrigin != "" {
		return s.publicOrigin
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func hostOnly(hostport string) string {
	if i := strings.LastIndex(hostport, ":"); i >= 0 && !strings.HasSuffix(hostport, "]") {
		if !strings.Contains(hostport[i:], "]") {
			hostport = hostport[:i]
		}
	}
	return strings.Trim(hostport, "[]")
}

// --- namespace resolution against the relay ---

type resolveStatus int

const (
	resolveOK resolveStatus = iota
	resolvePending
	resolveConflict
	resolveError
)

// resolveNamespace asks the relay to map the principal to a namespace.
// inviteCode is passed through on redemption attempts.
func (s *Server) resolveNamespace(p *principal, inviteCode string) (ns, role string, status resolveStatus, detail string) {
	var body any
	if p.Provider == providerHeader {
		body = map[string]string{"identity": p.Subject}
	} else {
		m := map[string]string{"provider": p.Provider, "subject": p.Subject, "login": p.Login, "name": p.Name}
		if inviteCode != "" {
			m["invite_code"] = inviteCode
		}
		body = m
	}
	resp, err := s.adminReq("POST", "/admin/resolve-user", nil, body)
	if err != nil {
		return "", "", resolveError, "relay unreachable: " + err.Error()
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var out struct{ Namespace, Role string }
		json.NewDecoder(resp.Body).Decode(&out)
		if out.Namespace == "" {
			return "", "", resolveError, "could not resolve namespace"
		}
		return out.Namespace, out.Role, resolveOK, ""
	case http.StatusForbidden:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if strings.TrimSpace(string(b)) == pendingInviteBody {
			return "", "", resolvePending, ""
		}
		return "", "", resolveError, "relay admin error"
	case http.StatusConflict:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		reason := strings.TrimSpace(string(b))
		if reason == "" {
			reason = "identity maps to an existing namespace"
		}
		return "", "", resolveConflict, reason
	default:
		return "", "", resolveError, fmt.Sprintf("relay admin error (status %d)", resp.StatusCode)
	}
}

// pageAuth authenticates a browser page load, redirecting through the login
// and pending pages as needed. ok=false means a response was already written.
func (s *Server) pageAuth(w http.ResponseWriter, r *http.Request, next string) (string, bool) {
	if s.relayURL == "" || s.adminSecret == "" {
		http.Error(w, "portal not wired yet (set RELAY_ADMIN_URL and WANCTL_ADMIN_SECRET)", http.StatusServiceUnavailable)
		return "", false
	}
	p := s.principalFrom(r)
	if p == nil {
		if s.oauthEnabled() {
			http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(safeNext(next)), http.StatusSeeOther)
		} else {
			http.Error(w, "no SSO identity header ("+s.userHeader+"); open /whoami to find the right header", http.StatusUnauthorized)
		}
		return "", false
	}
	ns, _, status, detail := s.resolveNamespace(p, "")
	switch status {
	case resolveOK:
		return ns, true
	case resolvePending:
		http.Redirect(w, r, "/pending", http.StatusSeeOther)
	case resolveConflict:
		http.Error(w, detail, http.StatusConflict)
	default:
		http.Error(w, detail, http.StatusBadGateway)
	}
	return "", false
}

// --- pending / invite redemption ---

func (s *Server) handlePending(w http.ResponseWriter, r *http.Request) {
	if !s.oauthEnabled() {
		http.NotFound(w, r)
		return
	}
	p := s.principalFrom(r)
	if p == nil {
		http.Redirect(w, r, "/auth/login?next=/pending", http.StatusSeeOther)
		return
	}
	// Already admitted? Straight home.
	if _, _, status, _ := s.resolveNamespace(p, ""); status == resolveOK {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, pendingPage, htmlEscape(p.Login))
}

func (s *Server) handleAuthRedeem(w http.ResponseWriter, r *http.Request) {
	if !s.oauthEnabled() {
		http.NotFound(w, r)
		return
	}
	p := s.principalFrom(r)
	if p == nil {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}
	var in struct {
		Code string `json:"code"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	in.Code = strings.TrimSpace(in.Code)
	if in.Code == "" {
		http.Error(w, "empty invite code", http.StatusBadRequest)
		return
	}
	ns, role, status, detail := s.resolveNamespace(p, in.Code)
	switch status {
	case resolveOK:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"namespace": ns, "role": role})
	case resolvePending:
		http.Error(w, "invite code not accepted", http.StatusForbidden)
	case resolveConflict:
		http.Error(w, detail, http.StatusConflict)
	default:
		http.Error(w, detail, http.StatusBadGateway)
	}
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}

// pendingPage matches the enroll page's cream styling: same product, same room.
const pendingPage = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>等待邀请 · wanctl</title><style>
:root{--cream:#FBF6EC;--ink:#2c2a26;--brand:#E08D3C;--soft:#efe7d6}
*{box-sizing:border-box}body{margin:0;font-family:-apple-system,system-ui,"PingFang SC",sans-serif;
background:var(--cream);color:var(--ink);display:flex;min-height:100vh;align-items:center;justify-content:center}
.card{background:#fff;border:1px solid var(--soft);border-radius:18px;padding:40px 44px;max-width:440px;
box-shadow:0 8px 30px rgba(0,0,0,.06);text-align:center}
h1{font-size:20px;margin:0 0 6px}
.sub{color:#8a857c;font-size:14px;margin-bottom:26px}
input{width:100%%;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:16px;padding:12px 14px;
border:2px dashed var(--brand);border-radius:12px;background:var(--cream);color:var(--ink);text-align:center;outline:none}
button{margin-top:14px;width:100%%;padding:12px;border:0;border-radius:12px;background:var(--brand);color:#fff;
font-size:15px;font-weight:600;cursor:pointer}
.err{margin-top:12px;font-size:13px;color:#c0392b;min-height:18px}
.tip{margin-top:18px;font-size:12px;color:#aaa39a;line-height:1.7}
a{color:var(--brand)}</style></head><body><div class="card">
<h1>等待邀请</h1>
<div class="sub">你已用 GitHub 账号 <b>%s</b> 登录，但这个 wanctl 实例是邀请制的。</div>
<input id="code" placeholder="粘贴邀请码 winv_…" autocomplete="off">
<button id="go">兑换邀请码</button>
<div class="err" id="err"></div>
<div class="tip">没有邀请码？请联系这个实例的管理员为你创建邀请，<br>或预先登记你的 GitHub 用户名。</div>
</div><script>
function csrf(){var m=document.cookie.match(/(?:^|; )wanctl_csrf=([^;]*)/);return m?decodeURIComponent(m[1]):""}
document.getElementById('go').onclick=function(){
  var code=document.getElementById('code').value.trim();
  var err=document.getElementById('err');
  if(!code){err.textContent='请先粘贴邀请码';return}
  fetch('/auth/redeem',{method:'POST',headers:{'Content-Type':'application/json','X-CSRF-Token':csrf()},
    body:JSON.stringify({code:code})}).then(function(r){
    if(r.ok){location.href='/'}else{r.text().then(function(t){err.textContent=t||('兑换失败 ('+r.status+')')})}
  }).catch(function(){err.textContent='网络错误，请重试'});
};
</script></body></html>`
