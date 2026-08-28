package relay

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"wanctl/internal/transport"
)

// The portal's console-admin identity rides along with the enrollment so a fresh
// device can accept the portal's web console without a hand-passed --portal-fps.
func TestEnrollExchangeReturnsPortalFingerprint(t *testing.T) {
	fp := transport.Fingerprint([]byte("portal cert"))
	r := New(EnvTokenStore(""))
	r.SetAdmin(&issuingAdmin{})
	r.enrollCodes["GOOD-2345"] = &enrollCode{
		namespace: "alice", portalFP: fp,
		expires: time.Now().Add(time.Minute),
	}

	w := exchange(t, r, "GOOD-2345")
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var out struct {
		Token     string `json:"token"`
		Namespace string `json:"namespace"`
		PortalFP  string `json:"portal_fp"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.PortalFP != fp {
		t.Fatalf("portal_fp = %q, want %q", out.PortalFP, fp)
	}
}

// A malformed fingerprint would be seeded into a device's console-admin set,
// where it can only fail closed — reject it where it enters instead.
func TestParseMintBodyValidatesPortalFingerprint(t *testing.T) {
	good := transport.Fingerprint([]byte("portal cert"))
	if ns, fp, err := parseMintBody(strings.NewReader(`{"Namespace":"alice","portal_fp":"` + good + `"}`)); err != nil || ns != "alice" || fp != good {
		t.Fatalf("valid body: ns=%q fp=%q err=%v", ns, fp, err)
	}
	if _, _, err := parseMintBody(strings.NewReader(`{"Namespace":"alice","portal_fp":"not-a-fingerprint"}`)); err == nil {
		t.Fatal("malformed portal_fp accepted")
	}
	// Omitting it stays legal: older portals, and any non-portal caller.
	if _, fp, err := parseMintBody(strings.NewReader(`{"Namespace":"alice"}`)); err != nil || fp != "" {
		t.Fatalf("absent portal_fp: fp=%q err=%v", fp, err)
	}
	if _, _, err := parseMintBody(strings.NewReader(`{}`)); err == nil {
		t.Fatal("missing namespace accepted")
	}
}
