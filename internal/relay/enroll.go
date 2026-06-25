package relay

import (
	"crypto/rand"
	"encoding/json"
	"net/http"
	"time"
)

// Device enrollment (OAuth-style). The portal authenticates a human via Feishu
// SSO, then calls /admin/enroll/mint to get a short one-time code bound to a
// freshly issued namespace token. The human pastes the code into `wanctl`, which
// trades it at the public /enroll/exchange endpoint for that token. Codes are
// in-memory, single-use, and short-lived; losing them on relay restart only
// forces re-issuing (no security impact).

const enrollCodeTTL = 5 * time.Minute

type enrollCode struct {
	token     string
	namespace string
	expires   time.Time
}

// newEnrollCode returns a human-friendly XXXX-XXXX code from an unambiguous
// alphabet (no 0/O/1/I/L).
func newEnrollCode() string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 8)
	rand.Read(b)
	out := make([]byte, 0, 9)
	for i, c := range b {
		if i == 4 {
			out = append(out, '-')
		}
		out = append(out, alphabet[int(c)%len(alphabet)])
	}
	return string(out)
}

// handleEnrollMint (admin-gated) issues a namespace token and returns a one-time
// code mapping to it. Called by the portal after SSO.
func (r *Relay) handleEnrollMint(w http.ResponseWriter, req *http.Request) {
	if !r.adminOK(req) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.admin == nil {
		http.Error(w, "no admin store", http.StatusServiceUnavailable)
		return
	}
	var body struct{ Namespace string }
	json.NewDecoder(req.Body).Decode(&body)
	if body.Namespace == "" {
		http.Error(w, "namespace required", http.StatusBadRequest)
		return
	}
	token, err := r.admin.IssueToken(body.Namespace, "device-enroll", 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	code := newEnrollCode()
	r.enrollMu.Lock()
	r.purgeEnrollLocked()
	r.enrollCodes[code] = &enrollCode{token: token, namespace: body.Namespace, expires: time.Now().Add(enrollCodeTTL)}
	r.enrollMu.Unlock()
	writeJSON(w, map[string]any{"code": code, "expires_in": int(enrollCodeTTL.Seconds())})
}

// handleEnrollExchange (public) trades a valid unused code for its token.
func (r *Relay) handleEnrollExchange(w http.ResponseWriter, req *http.Request) {
	var body struct{ Code string }
	json.NewDecoder(req.Body).Decode(&body)
	if body.Code == "" {
		http.Error(w, "code required", http.StatusBadRequest)
		return
	}
	r.enrollMu.Lock()
	r.purgeEnrollLocked()
	ec := r.enrollCodes[body.Code]
	if ec != nil {
		delete(r.enrollCodes, body.Code) // single-use
	}
	r.enrollMu.Unlock()
	if ec == nil || time.Now().After(ec.expires) {
		http.Error(w, "invalid or expired code", http.StatusUnauthorized)
		return
	}
	writeJSON(w, map[string]string{"token": ec.token, "namespace": ec.namespace})
}

// purgeEnrollLocked drops expired codes. Caller holds enrollMu.
func (r *Relay) purgeEnrollLocked() {
	now := time.Now()
	for k, v := range r.enrollCodes {
		if now.After(v.expires) {
			delete(r.enrollCodes, k)
		}
	}
}
