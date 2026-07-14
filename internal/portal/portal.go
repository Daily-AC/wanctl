// Package portal is the team web app (humans, behind thunderbox Feishu SSO). It
// identifies the logged-in user from an SSO-injected request header, resolves a
// namespace, and proxies token/device/ACL/audit operations to the relay's
// secret-gated admin API. It holds no database of its own — the relay owns the
// Postgres — so it needs only the relay URL and a shared admin secret.
package portal

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"wanctl/internal/client"
	"wanctl/internal/transport"
)

//go:embed index.html
var assets embed.FS

// skillURL is the canonical public install URL for the wanctl SKILL. The portal
// is SSO-gated, so AI clients (which have no session) cannot fetch directly
// from this domain — the skill lives on the relay (public) and the portal /skills
// path 302's to it for discoverability from the browser.
const skillURL = "https://wanctl-relay.***REMOVED***.***REMOVED***.com/skills"

// Config holds all parameters for New.
type Config struct {
	RelayAdminURL string // https://relay  (admin /admin/* proxy target)
	AdminSecret   string
	UserHeader    string
	RelayDialURL  string // ws(s)://relay  (controller dial target for console sessions)
	PortalToken   string // privileged token whose namespace == relay portalNS
	Transport     string // "ws" or "http"
	Identity      *transport.Identity
	Known         *transport.Store
}

// Server is the portal web app.
type Server struct {
	relayURL    string
	adminSecret string
	userHeader  string
	hc          *http.Client

	dialer *client.Client // controller used to open console sessions

	mu    sync.Mutex
	conns map[string]*deviceConn // key "ns/device"
}

// New configures the portal. With an empty relayURL/secret the server still
// starts so / and /whoami work (for SSO-header discovery); data endpoints then
// return 503 until configured.
func New(cfg Config) *Server {
	uh := cfg.UserHeader
	if uh == "" {
		uh = "X-Auth-Request-Email"
	}
	s := &Server{
		relayURL:    strings.TrimRight(cfg.RelayAdminURL, "/"),
		adminSecret: cfg.AdminSecret,
		userHeader:  uh,
		hc:          &http.Client{Timeout: 15 * time.Second},
		conns:       map[string]*deviceConn{},
	}
	if cfg.Identity != nil && cfg.RelayDialURL != "" && cfg.PortalToken != "" {
		s.dialer = client.NewWith(cfg.Identity, cfg.Known, cfg.RelayDialURL, cfg.PortalToken, cfg.Transport)
	}
	return s
}

// Handler returns the portal mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/whoami", s.handleWhoami)
	mux.HandleFunc("/enroll", s.handleEnroll)
	mux.HandleFunc("/skills", s.handleSkills)
	mux.HandleFunc("/api/me", s.handleMe)
	mux.HandleFunc("/api/tokens", s.handleTokens)
	mux.HandleFunc("/api/tokens/revoke", s.handleTokenRevoke)
	mux.HandleFunc("/api/devices", s.handleDevices)
	mux.HandleFunc("/api/namespaces", s.handleNamespaces)
	mux.HandleFunc("/api/acl", s.handleACL)
	mux.HandleFunc("/api/acl/revoke", s.handleACLRevoke)
	mux.HandleFunc("/api/audit", s.handleAudit)
	mux.HandleFunc("/api/devices/console", s.handleDeviceConsole)
	mux.HandleFunc("/api/devices/decide", s.handleDeviceDecide)
	mux.HandleFunc("/api/devices/pair", s.handleDevicePair)
	mux.HandleFunc("/api/devices/untrust", s.handleDeviceUntrust)
	mux.HandleFunc("/api/devices/remove", s.handleDeviceRemove)
	mux.HandleFunc("/api/devices/rules", s.handleDeviceRules)
	mux.HandleFunc("/api/devices/mode", s.handleDeviceMode)
	mux.HandleFunc("/api/devices/lan", s.handleDeviceLan)
	mux.HandleFunc("/api/devices/logs", s.handleDeviceLogs)
	mux.HandleFunc("/api/devices/events", s.handleDeviceEvents)
	mux.HandleFunc("/api/docs/tree", s.handleDocsTree)
	mux.HandleFunc("/api/docs/article/", s.handleDocsArticleGet)
	mux.HandleFunc("/api/docs/articles", s.handleDocsArticleWrite)
	mux.HandleFunc("/api/docs/articles/delete", s.handleDocsArticleDelete)
	mux.HandleFunc("/api/docs/groups", s.handleDocsGroupWrite)
	mux.HandleFunc("/api/docs/groups/delete", s.handleDocsGroupDelete)
	return mux
}

// --- docs ---
//
// Tree and per-article reads hit the relay's public endpoints (no auth, no
// admin secret). Writes require a logged-in SSO user — the portal resolves the
// SSO header to a namespace, then forwards to the relay's admin-gated mirror
// with that namespace stamped as the article author.

func (s *Server) handleDocsTree(w http.ResponseWriter, r *http.Request) {
	s.proxyRelayPublic(w, "/docs/tree.json")
}

func (s *Server) handleDocsArticleGet(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimPrefix(r.URL.Path, "/api/docs/article/")
	if slug == "" {
		http.NotFound(w, r)
		return
	}
	s.proxyRelayPublic(w, "/docs/"+slug+".json")
}

func (s *Server) handleDocsArticleWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	ns, ok := s.requireNS(w, r)
	if !ok {
		return
	}
	s.docsForwardAdmin(w, r, "/admin/docs/articles", ns)
}

func (s *Server) handleDocsArticleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.requireNS(w, r); !ok {
		return
	}
	s.docsForwardAdmin(w, r, "/admin/docs/articles/delete", "")
}

func (s *Server) handleDocsGroupWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	ns, ok := s.requireNS(w, r)
	if !ok {
		return
	}
	s.docsForwardAdmin(w, r, "/admin/docs/groups", ns)
}

func (s *Server) handleDocsGroupDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.requireNS(w, r); !ok {
		return
	}
	s.docsForwardAdmin(w, r, "/admin/docs/groups/delete", "")
}

// docsForwardAdmin merges the SSO-resolved namespace into the JSON body as
// "author" and forwards to the relay admin endpoint with the shared secret.
func (s *Server) docsForwardAdmin(w http.ResponseWriter, r *http.Request, path, author string) {
	body := map[string]any{}
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&body)
	}
	if author != "" {
		body["author"] = author
	}
	resp, err := s.adminReq("POST", path, nil, body)
	if err != nil {
		http.Error(w, "relay unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyResp(w, resp)
}

// proxyRelayPublic forwards a GET to a relay public endpoint (no auth header).
// Used for docs reads, which are public.
func (s *Server) proxyRelayPublic(w http.ResponseWriter, path string) {
	if s.relayURL == "" {
		http.Error(w, "portal not wired", http.StatusServiceUnavailable)
		return
	}
	req, _ := http.NewRequest("GET", s.relayURL+path, nil)
	resp, err := s.hc.Do(req)
	if err != nil {
		http.Error(w, "relay unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyResp(w, resp)
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

// handleSkills 302's to the relay's public /skills (which serves the canonical
// SKILL.md). The portal can't host it directly because its thunderbox app is
// SSO-gated and AI clients have no session.
func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, skillURL, http.StatusFound)
}

// handleWhoami dumps request headers so we can discover the SSO identity header.
func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "configured identity header: %q\nresolved identity: %q\n\nall request headers:\n", s.userHeader, r.Header.Get(s.userHeader))
	for k, v := range r.Header {
		fmt.Fprintf(w, "  %s: %s\n", k, strings.Join(v, ", "))
	}
}

func (s *Server) identity(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get(s.userHeader))
}

// adminReq calls the relay admin API with the shared secret.
func (s *Server) adminReq(method, path string, query url.Values, body any) (*http.Response, error) {
	u := s.relayURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Admin-Secret", s.adminSecret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return s.hc.Do(req)
}

// requireNS authenticates the caller and resolves their namespace via the relay.
// handleEnroll serves the device-enrollment page. The visitor is already
// authenticated by Feishu SSO, so we resolve their namespace, ask the relay to
// mint a one-time code bound to a fresh token, and show it for them to paste
// into `wanctl`.
func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.requireNS(w, r)
	if !ok {
		return
	}
	resp, err := s.adminReq("POST", "/admin/enroll/mint", nil, map[string]string{"namespace": ns})
	if err != nil {
		http.Error(w, "relay unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		http.Error(w, "mint failed: "+string(b), http.StatusBadGateway)
		return
	}
	var out struct {
		Code      string `json:"code"`
		ExpiresIn int    `json:"expires_in"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	mins := out.ExpiresIn / 60
	if mins < 1 {
		mins = 1
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, enrollPage, ns, out.Code, mins)
}

// enrollPage: %[1]s namespace, %[2]s one-time code, %[3]d minutes-to-expiry.
const enrollPage = `<!doctype html><html lang="zh"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>wanctl 设备授权</title><style>
:root{--cream:#FBF6EC;--ink:#2c2a26;--brand:#E08D3C;--soft:#efe7d6}
*{box-sizing:border-box}body{margin:0;font-family:-apple-system,system-ui,"PingFang SC",sans-serif;
background:var(--cream);color:var(--ink);display:flex;min-height:100vh;align-items:center;justify-content:center}
.card{background:#fff;border:1px solid var(--soft);border-radius:18px;padding:40px 44px;max-width:440px;
box-shadow:0 8px 30px rgba(0,0,0,.06);text-align:center}
h1{font-size:20px;margin:0 0 6px}.sub{color:#8a857c;font-size:14px;margin-bottom:26px}
.code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:38px;letter-spacing:4px;font-weight:700;
color:var(--brand);background:var(--cream);border:2px dashed var(--brand);border-radius:14px;padding:18px 10px;cursor:pointer;user-select:all}
.copy{margin-top:14px;font-size:13px;color:#8a857c}.ns{font-weight:600}
.steps{text-align:left;margin:26px 0 0;padding:18px;background:var(--cream);border-radius:12px;font-size:13px;line-height:1.7;color:#6b665d}
.tip{margin-top:18px;font-size:12px;color:#aaa39a}b{color:var(--ink)}
</style></head><body><div class="card">
<h1>🛡️ 设备授权</h1>
<div class="sub">空间 <span class="ns">%[1]s</span> · 把下面的 code 贴回终端</div>
<div class="code" onclick="navigator.clipboard&&navigator.clipboard.writeText(this.textContent.trim());document.querySelector('.copy').textContent='✓ 已复制'">%[2]s</div>
<div class="copy">点一下复制 · %[3]d 分钟内有效 · 仅可用一次</div>
<div class="steps">回到终端,在 <b>输入授权 code:</b> 后粘贴上面的 code,回车即可。<br>设备会自动绑定到空间 <b>%[1]s</b> 并转入后台运行。</div>
<div class="tip">这台机器就成了一个可被远程控制的设备。停止用 <b>wanctl stop</b>。</div>
</div></body></html>`

func (s *Server) requireNS(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.relayURL == "" || s.adminSecret == "" {
		http.Error(w, "portal not wired yet (set RELAY_ADMIN_URL and WANCTL_ADMIN_SECRET)", http.StatusServiceUnavailable)
		return "", false
	}
	id := s.identity(r)
	if id == "" {
		http.Error(w, "no SSO identity header ("+s.userHeader+"); open /whoami to find the right header", http.StatusUnauthorized)
		return "", false
	}
	resp, err := s.adminReq("POST", "/admin/resolve-user", nil, map[string]string{"identity": id})
	if err != nil {
		http.Error(w, "relay unreachable: "+err.Error(), http.StatusBadGateway)
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		http.Error(w, "relay admin error", resp.StatusCode)
		return "", false
	}
	var out struct{ Namespace string }
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Namespace == "" {
		http.Error(w, "could not resolve namespace", http.StatusInternalServerError)
		return "", false
	}
	return out.Namespace, true
}

// proxyGet forwards a GET to the relay admin (scoped by namespace) to the client.
func (s *Server) proxyGet(w http.ResponseWriter, ns, path string) {
	resp, err := s.adminReq("GET", path, url.Values{"namespace": {ns}}, nil)
	if err != nil {
		http.Error(w, "relay unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyResp(w, resp)
}

// proxyPost merges the namespace into the client's JSON body and forwards a POST.
func (s *Server) proxyPost(w http.ResponseWriter, r *http.Request, ns, path string) {
	body := map[string]any{}
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&body)
	}
	body["namespace"] = ns
	resp, err := s.adminReq("POST", path, nil, body)
	if err != nil {
		http.Error(w, "relay unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyResp(w, resp)
}

func copyResp(w http.ResponseWriter, resp *http.Response) {
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.requireNS(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"identity": s.identity(r), "namespace": ns})
}

func (s *Server) handleTokens(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.requireNS(w, r)
	if !ok {
		return
	}
	if r.Method == "POST" {
		s.proxyPost(w, r, ns, "/admin/tokens/issue")
		return
	}
	s.proxyGet(w, ns, "/admin/tokens")
}

func (s *Server) handleTokenRevoke(w http.ResponseWriter, r *http.Request) {
	if ns, ok := s.requireNS(w, r); ok {
		s.proxyPost(w, r, ns, "/admin/tokens/revoke")
	}
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	if ns, ok := s.requireNS(w, r); ok {
		s.proxyGet(w, ns, "/admin/devices")
	}
}

func (s *Server) handleNamespaces(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireNS(w, r); !ok {
		return
	}
	resp, err := s.adminReq("GET", "/admin/users", nil, nil)
	if err != nil {
		http.Error(w, "relay unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyResp(w, resp)
}

func (s *Server) handleACL(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.requireNS(w, r)
	if !ok {
		return
	}
	if r.Method == "POST" {
		s.proxyPost(w, r, ns, "/admin/acl")
		return
	}
	s.proxyGet(w, ns, "/admin/acl")
}

func (s *Server) handleACLRevoke(w http.ResponseWriter, r *http.Request) {
	if ns, ok := s.requireNS(w, r); ok {
		s.proxyPost(w, r, ns, "/admin/acl/revoke")
	}
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	if ns, ok := s.requireNS(w, r); ok {
		s.proxyGet(w, ns, "/admin/audit")
	}
}

// requireDevice authenticates the user, resolves their namespace, and verifies
// the named device is visible to them. It returns the device owner's namespace,
// because console dials must target owner/device even for ACL-shared devices.
func (s *Server) requireDevice(w http.ResponseWriter, r *http.Request, device string) (string, bool, bool) {
	ns, ok := s.requireNS(w, r)
	if !ok {
		return "", false, false
	}
	if device == "" {
		http.Error(w, "missing device", http.StatusBadRequest)
		return "", false, false
	}
	resp, err := s.adminReq("GET", "/admin/devices", url.Values{"namespace": {ns}}, nil)
	if err != nil {
		http.Error(w, "relay unreachable", http.StatusBadGateway)
		return "", false, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "relay admin error", resp.StatusCode)
		return "", false, false
	}
	var out struct {
		Devices []struct {
			Name   string `json:"name"`
			Owner  string `json:"owner"`
			Shared bool   `json:"shared"`
		} `json:"devices"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	matches := []struct {
		Name   string `json:"name"`
		Owner  string `json:"owner"`
		Shared bool   `json:"shared"`
	}{}
	for _, d := range out.Devices {
		if d.Name == device {
			if d.Owner == "" {
				d.Owner = ns
			}
			matches = append(matches, d)
		}
	}
	if len(matches) == 1 {
		return matches[0].Owner, matches[0].Shared || matches[0].Owner != ns, true
	}
	for _, d := range matches {
		if d.Owner == ns {
			return d.Owner, false, true
		}
	}
	if len(matches) > 1 {
		http.Error(w, "设备名有歧义，请用 CLI 指定 owner/device", http.StatusConflict)
		return "", false, false
	}
	http.Error(w, "device not in your namespace", http.StatusForbidden)
	return "", false, false
}

// requireOwnedConsole is requireDevice plus a write gate: ACL-shared devices
// are strictly read-only in the portal. Approvals, pairing, trust, rules, mode
// and LAN switches belong to the device owner alone — a grantee's perms (e.g.
// exec) never extend to driving the device's policy console.
func (s *Server) requireOwnedConsole(w http.ResponseWriter, r *http.Request, device string) (string, bool) {
	ns, shared, ok := s.requireDevice(w, r, device)
	if !ok {
		return "", false
	}
	if shared {
		http.Error(w, "共享设备只读：审批、规则、模式等设置只能由设备主人操作", http.StatusForbidden)
		return "", false
	}
	return ns, true
}

// deviceConnFor returns a warm console connection to ns/device, dialing if needed.
// It uses double-checked locking so that two concurrent callers for the same absent
// device (e.g. /api/devices/console and /api/devices/events on page load) do not
// both dial and leak the losing connection. The goroutine that loses the post-dial
// re-check closes its own conn and returns the winner's.
func (s *Server) deviceConnFor(ctx context.Context, ns, device string) (*deviceConn, error) {
	if s.dialer == nil {
		return nil, fmt.Errorf("portal console not wired (set WANCTL_RELAY, WANCTL_PORTAL_TOKEN)")
	}
	key := ns + "/" + device
	s.mu.Lock()
	if d := s.conns[key]; d != nil {
		if d.alive() {
			s.mu.Unlock()
			return d, nil
		}
		// Stale (device restarted / dropped). Evict and re-dial.
		d.close()
		delete(s.conns, key)
	}
	s.mu.Unlock()
	conn, err := s.dialer.OpenConsole(ctx, key)
	if err != nil {
		return nil, err
	}
	d := newDeviceConn(conn)
	s.mu.Lock()
	if existing := s.conns[key]; existing != nil && existing.alive() {
		s.mu.Unlock()
		d.close() // lost the race: close our dial, use the winner
		return existing, nil
	}
	s.conns[key] = d
	s.mu.Unlock()
	return d, nil
}

func (s *Server) dropConn(ns, device string) {
	key := ns + "/" + device
	s.mu.Lock()
	if d := s.conns[key]; d != nil {
		d.close()
		delete(s.conns, key)
	}
	s.mu.Unlock()
}

func (s *Server) handleDeviceConsole(w http.ResponseWriter, r *http.Request) {
	device := r.URL.Query().Get("device")
	ns, _, ok := s.requireDevice(w, r, device)
	if !ok {
		return
	}
	d, err := s.deviceConnFor(r.Context(), ns, device)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	st, err := d.state()
	if err != nil {
		s.dropConn(ns, device)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	json.NewEncoder(w).Encode(st)
}

func (s *Server) handleDeviceDecide(w http.ResponseWriter, r *http.Request) {
	var body struct{ Device, ID, Verdict string }
	json.NewDecoder(r.Body).Decode(&body)
	ns, ok := s.requireOwnedConsole(w, r, body.Device)
	if !ok {
		return
	}
	d, err := s.deviceConnFor(r.Context(), ns, body.Device)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if err := d.decide(body.ID, body.Verdict, "portal:"+s.identity(r)); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleDevicePair trusts or denies a pending controller pairing on the device.
func (s *Server) handleDevicePair(w http.ResponseWriter, r *http.Request) {
	var body struct{ Device, FP, Verdict string }
	json.NewDecoder(r.Body).Decode(&body)
	ns, ok := s.requireOwnedConsole(w, r, body.Device)
	if !ok {
		return
	}
	d, err := s.deviceConnFor(r.Context(), ns, body.Device)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if err := d.pairDecide(body.FP, body.Verdict); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleDeviceUntrust drops a trusted controller from the device.
func (s *Server) handleDeviceUntrust(w http.ResponseWriter, r *http.Request) {
	var body struct{ Device, FP string }
	json.NewDecoder(r.Body).Decode(&body)
	ns, ok := s.requireOwnedConsole(w, r, body.Device)
	if !ok {
		return
	}
	d, err := s.deviceConnFor(r.Context(), ns, body.Device)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if err := d.untrust(body.FP); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleDeviceRemove unbinds a device from the namespace (relay-side record).
func (s *Server) handleDeviceRemove(w http.ResponseWriter, r *http.Request) {
	var body struct{ Device string }
	json.NewDecoder(r.Body).Decode(&body)
	ns, shared, ok := s.requireDevice(w, r, body.Device)
	if !ok {
		return
	}
	if shared {
		http.Error(w, "只能解绑自己的设备", http.StatusForbidden)
		return
	}
	s.dropConn(ns, body.Device) // close any cached console session to it
	resp, err := s.adminReq("POST", "/admin/devices/remove", nil, map[string]string{"namespace": ns, "device": body.Device})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		http.Error(w, "remove failed: "+string(b), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleDeviceRules(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Device, Op, Kind, Pattern, Dir, Scope string
		Index                                 int
	}
	json.NewDecoder(r.Body).Decode(&body)
	ns, ok := s.requireOwnedConsole(w, r, body.Device)
	if !ok {
		return
	}
	d, err := s.deviceConnFor(r.Context(), ns, body.Device)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	var err2 error
	if body.Op == "rm" {
		err2 = d.removeRule(body.Index)
	} else {
		err2 = d.addRule(body.Kind, body.Pattern, body.Dir, body.Scope)
	}
	if err2 != nil {
		http.Error(w, err2.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleDeviceLan toggles the device's intranet fast-path uplink from the web
// console (POST {device, on}).
func (s *Server) handleDeviceLan(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Device string
		On     bool
	}
	json.NewDecoder(r.Body).Decode(&body)
	ns, ok := s.requireOwnedConsole(w, r, body.Device)
	if !ok {
		return
	}
	d, err := s.deviceConnFor(r.Context(), ns, body.Device)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if err := d.setLan(body.On); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleDeviceMode(w http.ResponseWriter, r *http.Request) {
	var body struct{ Device, Mode string }
	json.NewDecoder(r.Body).Decode(&body)
	ns, ok := s.requireOwnedConsole(w, r, body.Device)
	if !ok {
		return
	}
	d, err := s.deviceConnFor(r.Context(), ns, body.Device)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if err := d.setMode(body.Mode); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleDeviceLogs pulls the device's JSONL activity log (exec/file events with
// decision and exit code) over the console session and forwards it as
// {"logs":[...]} for the SPA's activity timeline.
func (s *Server) handleDeviceLogs(w http.ResponseWriter, r *http.Request) {
	device := r.URL.Query().Get("device")
	ns, _, ok := s.requireDevice(w, r, device)
	if !ok {
		return
	}
	d, err := s.deviceConnFor(r.Context(), ns, device)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	raw, err := d.logs(r.URL.Query().Get("type"), r.URL.Query().Get("grep"), r.URL.Query().Get("since"), 200)
	if err != nil {
		s.dropConn(ns, device)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"logs":`))
	w.Write(raw)
	w.Write([]byte(`}`))
}

// eventPollWait bounds one long-poll on /api/devices/events. Shorter than the
// relay's own poll windows so the request returns a finite response well before
// any proxy idle timeout; the browser immediately re-polls.
const eventPollWait = 25 * time.Second

func (s *Server) handleDeviceEvents(w http.ResponseWriter, r *http.Request) {
	device := r.URL.Query().Get("device")
	ns, _, ok := s.requireDevice(w, r, device)
	if !ok {
		return
	}
	d, err := s.deviceConnFor(r.Context(), ns, device)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// Long-poll, not SSE: thunderbox's nginx buffers streaming responses (and
	// ignores X-Accel-Buffering), so an open text/event-stream never reaches the
	// browser. Block for one approval-state push (or time out), return a finite
	// JSON response nginx forwards promptly, and let the client re-poll.
	select {
	case <-r.Context().Done():
		w.WriteHeader(http.StatusNoContent)
	case st := <-d.notifs():
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(st)
	case <-time.After(eventPollWait):
		w.WriteHeader(http.StatusNoContent) // no event this round; client re-polls
	}
}
