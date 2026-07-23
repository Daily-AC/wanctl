// Package admission handles relay admission credentials consistently across
// HTTP and WebSocket handshakes.
package admission

import (
	"net/http"
	"strings"
)

const legacyWarning = `299 wanctl "token query authentication is deprecated; use Authorization: Bearer"`

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

// Token returns the bearer credential. A malformed Authorization header fails
// closed and never falls back to the legacy query parameter.
func Token(req *http.Request) (token string, legacy, ok bool) {
	if value := req.Header.Get("Authorization"); value != "" {
		parts := strings.Fields(value)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
			return "", false, false
		}
		return parts[1], false, true
	}
	if token := req.URL.Query().Get("token"); token != "" {
		return token, true, true
	}
	return "", false, false
}

func MarkLegacy(w http.ResponseWriter) {
	w.Header().Set("Deprecation", "true")
	w.Header().Set("Warning", legacyWarning)
}
