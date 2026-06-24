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
	mux.HandleFunc("/api/me", s.handleMe)
	mux.HandleFunc("/api/tokens", s.handleTokens)
	mux.HandleFunc("/api/tokens/revoke", s.handleTokenRevoke)
	mux.HandleFunc("/api/devices", s.handleDevices)
	mux.HandleFunc("/api/acl", s.handleACL)
	mux.HandleFunc("/api/acl/revoke", s.handleACLRevoke)
	mux.HandleFunc("/api/audit", s.handleAudit)
	mux.HandleFunc("/api/devices/console", s.handleDeviceConsole)
	mux.HandleFunc("/api/devices/decide", s.handleDeviceDecide)
	mux.HandleFunc("/api/devices/rules", s.handleDeviceRules)
	mux.HandleFunc("/api/devices/mode", s.handleDeviceMode)
	mux.HandleFunc("/api/devices/logs", s.handleDeviceLogs)
	mux.HandleFunc("/api/devices/events", s.handleDeviceEvents)
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
// the named device belongs to it (queried from the relay admin device list).
// Security precondition: the relay's /admin/devices endpoint honours the
// namespace query parameter (WHERE owner_namespace = $1), so the returned list
// contains only the caller-namespace's devices and a foreign device name is
// never matched.
func (s *Server) requireDevice(w http.ResponseWriter, r *http.Request, device string) (string, bool) {
	ns, ok := s.requireNS(w, r)
	if !ok {
		return "", false
	}
	if device == "" {
		http.Error(w, "missing device", http.StatusBadRequest)
		return "", false
	}
	resp, err := s.adminReq("GET", "/admin/devices", url.Values{"namespace": {ns}}, nil)
	if err != nil {
		http.Error(w, "relay unreachable", http.StatusBadGateway)
		return "", false
	}
	defer resp.Body.Close()
	var out struct {
		Devices []struct{ Name string } `json:"devices"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	for _, d := range out.Devices {
		if d.Name == device {
			return ns, true
		}
	}
	http.Error(w, "device not in your namespace", http.StatusForbidden)
	return "", false
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
	ns, ok := s.requireDevice(w, r, device)
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
	ns, ok := s.requireDevice(w, r, body.Device)
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

func (s *Server) handleDeviceRules(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Device, Op, Kind, Pattern, Dir, Scope string
		Index                                 int
	}
	json.NewDecoder(r.Body).Decode(&body)
	ns, ok := s.requireDevice(w, r, body.Device)
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

func (s *Server) handleDeviceMode(w http.ResponseWriter, r *http.Request) {
	var body struct{ Device, Mode string }
	json.NewDecoder(r.Body).Decode(&body)
	ns, ok := s.requireDevice(w, r, body.Device)
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
	ns, ok := s.requireDevice(w, r, device)
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

func (s *Server) handleDeviceEvents(w http.ResponseWriter, r *http.Request) {
	device := r.URL.Query().Get("device")
	ns, ok := s.requireDevice(w, r, device)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no stream", http.StatusInternalServerError)
		return
	}
	d, err := s.deviceConnFor(r.Context(), ns, device)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprint(w, "data: hello\n\n")
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case st := <-d.notifs():
			b, _ := json.Marshal(st)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
	}
}
