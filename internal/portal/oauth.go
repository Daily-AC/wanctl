package portal

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// The human half of OAuth 2.1 for the hosted MCP endpoint.
//
// The relay owns everything a machine talks to — discovery, registration,
// tokens, revocation. This file owns the one leg a person is on: sign in, read
// who is asking, decide. It lives here because this is the service that already
// knows how to turn a GitHub login into a wanctl namespace, and because putting
// a login page on the relay would mean a second place that can mint a session.
// See docs/adr/0008-mcp-oauth-split-portal-relay.md.

// oauthAuthorizeParams are the query parameters an OAuth client sends. They are
// forwarded to the relay verbatim; the portal validates none of them itself,
// because a second implementation of those rules is a second thing to get wrong.
var oauthAuthorizeParams = []string{
	"client_id", "redirect_uri", "response_type", "code_challenge",
	"code_challenge_method", "state", "scope", "resource",
}

// maxOAuthParam bounds one parameter. The longest legitimate value is a
// redirect_uri or an opaque state; anything past this is not a real client.
const maxOAuthParam = 2048

func (s *Server) handleOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	in := r.URL.Query()
	forward := map[string]string{}
	query := url.Values{}
	for _, name := range oauthAuthorizeParams {
		v := in.Get(name)
		if len(v) > maxOAuthParam {
			s.renderOAuthError(w, http.StatusBadRequest, name+" is too long")
			return
		}
		forward[name] = v
		if v != "" {
			query.Set(name, v)
		}
	}
	// Sign-in comes first and returns here with the same query, so the client's
	// request survives the GitHub round trip untouched.
	ns, ok := s.pageAuth(w, r, "/oauth/authorize?"+query.Encode())
	if !ok {
		return
	}
	resp, err := s.adminReq(http.MethodPost, "/admin/oauth/authorize-request", nil, forward)
	if err != nil {
		s.renderOAuthError(w, http.StatusBadGateway, "relay unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// The relay refused the request itself — unknown client, unregistered
		// redirect_uri, no PKCE. Those must be shown here rather than redirected
		// back, or the page becomes an open redirect for anyone who can guess a
		// client_id.
		s.renderOAuthError(w, http.StatusBadRequest, oauthErrorText(resp.Body))
		return
	}
	var out struct {
		RequestID   string `json:"request_id"`
		ClientName  string `json:"client_name"`
		RedirectURI string `json:"redirect_uri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.RequestID == "" {
		s.renderOAuthError(w, http.StatusBadGateway, "the relay returned an unreadable authorization request")
		return
	}
	s.render(w, "oauth.html", map[string]any{
		"NS":         ns,
		"ClientName": out.ClientName,
		"RequestID":  out.RequestID,
		// The host, not the whole URI: the host is the part that answers "where
		// would this send my authorization", and a full URI with a long opaque
		// path invites people to stop reading it.
		"ClientHost": redirectHost(out.RedirectURI),
	})
}

// handleOAuthDecide is the only place a decision is recorded. It is a POST, so
// the portal's same-origin and double-submit CSRF checks apply: a page the user
// merely visited must not be able to approve a connector on their behalf.
func (s *Server) handleOAuthDecide(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.requireNS(w, r)
	if !ok {
		return
	}
	var body struct {
		RequestID string `json:"request_id"`
		Allow     bool   `json:"allow"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.RequestID == "" {
		http.Error(w, "request_id required", http.StatusBadRequest)
		return
	}
	path, payload := "/admin/oauth/deny", map[string]string{"request_id": body.RequestID}
	if body.Allow {
		path = "/admin/oauth/approve"
		payload["namespace"] = ns
	}
	resp, err := s.adminReq(http.MethodPost, path, nil, payload)
	if err != nil {
		http.Error(w, "relay unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyResp(w, resp)
}

func (s *Server) renderOAuthError(w http.ResponseWriter, status int, detail string) {
	s.renderStatus(w, status, "oauth.html", map[string]any{"Error": detail})
}

// oauthErrorText turns the relay's RFC 6749 error body into one sentence, and
// falls back to the raw body for anything it does not recognize.
func oauthErrorText(body io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(body, 4096))
	var e struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(b, &e) == nil && e.Description != "" {
		return e.Description
	}
	if text := strings.TrimSpace(string(b)); text != "" {
		return text
	}
	return "the relay refused this authorization request"
}

func redirectHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Host
}
