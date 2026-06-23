// Package portal is the team web app (humans, behind thunderbox Feishu SSO). It
// identifies the logged-in user from an SSO-injected request header, resolves a
// namespace, and proxies token/device/ACL/audit operations to the relay's
// secret-gated admin API. It holds no database of its own — the relay owns the
// Postgres — so it needs only the relay URL and a shared admin secret.
package portal

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

//go:embed index.html
var assets embed.FS

// Server is the portal web app.
type Server struct {
	relayURL    string // e.g. https://wanctl-relay.***REMOVED***.***REMOVED***.com
	adminSecret string
	userHeader  string
	hc          *http.Client
}

// New configures the portal. With an empty relayURL/secret the server still
// starts so / and /whoami work (for SSO-header discovery); data endpoints then
// return 503 until configured.
func New(relayURL, adminSecret, userHeader string) *Server {
	if userHeader == "" {
		userHeader = "X-Auth-Request-Email"
	}
	return &Server{
		relayURL:    strings.TrimRight(relayURL, "/"),
		adminSecret: adminSecret,
		userHeader:  userHeader,
		hc:          &http.Client{Timeout: 15 * time.Second},
	}
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
