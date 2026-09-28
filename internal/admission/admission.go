// Package admission handles relay admission credentials consistently across
// HTTP and WebSocket handshakes.
package admission

import (
	"net/http"
	"strings"
)

func Header(token string) http.Header {
	h := make(http.Header)
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	return h
}

func SetBearer(req *http.Request, token string) {
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// Token returns the bearer credential from the Authorization header, the only
// place one is accepted. A token in the URL query is ignored: a URL is copied
// into proxy access logs, browser history and Referer headers, and every
// wanctl client has sent the header instead for many releases.
func Token(req *http.Request) (token string, ok bool) {
	parts := strings.Fields(req.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}
