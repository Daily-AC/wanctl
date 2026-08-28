package relay

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"wanctl/internal/transport"
)

// Device enrollment (OAuth-style). The portal authenticates a human via Feishu
// SSO, then calls /admin/enroll/mint to get a short one-time code bound to a
// freshly issued namespace token. The human pastes the code into `wanctl`, which
// trades it at the public /enroll/exchange endpoint for that token. Codes are
// in-memory, single-use, and short-lived; losing them on relay restart only
// forces re-issuing (no security impact).

const enrollCodeTTL = 5 * time.Minute

type enrollCode struct {
	namespace string
	portalFP  string // portal's console-admin identity, as declared at mint time
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

// parseMintBody reads a mint request. The portal fingerprint is carried
// opaquely but not blindly: a malformed value would be seeded into a device's
// console-admin set, where it could only ever fail closed.
func parseMintBody(r io.Reader) (namespace, portalFP string, err error) {
	var body struct {
		Namespace string
		PortalFP  string `json:"portal_fp"`
	}
	json.NewDecoder(r).Decode(&body)
	if body.Namespace == "" {
		return "", "", errors.New("namespace required")
	}
	if body.PortalFP != "" && !transport.ValidFingerprint(body.PortalFP) {
		return "", "", errors.New("invalid portal_fp")
	}
	return body.Namespace, body.PortalFP, nil
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
	ns, portalFP, err := parseMintBody(req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// The token is NOT issued here. Minting is a GET on the portal's /enroll
	// page, so a cross-site navigation can trigger it; issuing at mint time
	// left a live, never-expiring namespace token in the database for every
	// drive-by or abandoned enrollment (audit 2026-08-28, SEC-C-04). The code
	// now stands for "a token may be issued to this namespace"; the token is
	// created only when the code is redeemed at /enroll/exchange.
	code := newEnrollCode()
	r.enrollMu.Lock()
	r.purgeEnrollLocked()
	r.enrollCodes[code] = &enrollCode{namespace: ns, portalFP: portalFP, expires: time.Now().Add(enrollCodeTTL)}
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
	if r.admin == nil {
		http.Error(w, "no admin store", http.StatusServiceUnavailable)
		return
	}
	// Issue the token now, on redemption, so an unredeemed code never leaves a
	// credential behind (SEC-C-04). Label it with the device the agent is about
	// to register under is not known yet, so keep the "device-enroll" label.
	token, err := r.admin.IssueToken(ec.namespace, "device-enroll", 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"token": token, "namespace": ec.namespace, "portal_fp": ec.portalFP})
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
