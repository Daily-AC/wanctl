package relay

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func exchange(t *testing.T, r *Relay, code string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/enroll/exchange", strings.NewReader(`{"code":"`+code+`"}`))
	w := httptest.NewRecorder()
	r.handleEnrollExchange(w, req)
	return w
}

func TestEnrollExchangeOneTimeAndExpiry(t *testing.T) {
	r := New(EnvTokenStore(""))
	r.SetAdmin(&issuingAdmin{})
	r.enrollCodes["GOOD-2345"] = &enrollCode{namespace: "alice", expires: time.Now().Add(time.Minute)}
	r.enrollCodes["OLD-6789"] = &enrollCode{namespace: "bob", expires: time.Now().Add(-time.Minute)}

	// Valid code -> a token issued now, for the code's namespace.
	w := exchange(t, r, "GOOD-2345")
	if w.Code != 200 {
		t.Fatalf("valid exchange status=%d body=%s", w.Code, w.Body.String())
	}
	var out struct{ Token, Namespace string }
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.Token != "issued:alice" || out.Namespace != "alice" {
		t.Fatalf("got %+v", out)
	}

	// Single-use: the same code must not work twice.
	if w2 := exchange(t, r, "GOOD-2345"); w2.Code == 200 {
		t.Fatal("enrollment code was reusable")
	}

	// Expired code rejected.
	if w3 := exchange(t, r, "OLD-6789"); w3.Code == 200 {
		t.Fatal("expired code accepted")
	}

	// Unknown code rejected.
	if w4 := exchange(t, r, "NOPE-0000"); w4.Code == 200 {
		t.Fatal("unknown code accepted")
	}
}

func TestNewEnrollCodeFormat(t *testing.T) {
	c := newEnrollCode()
	if len(c) != 9 || c[4] != '-' {
		t.Fatalf("bad code format: %q", c)
	}
}

// issuingAdmin is a noopAdmin that hands back a namespace-tagged token, so the
// enrollment exchange (which now issues on redemption, SEC-C-04) is testable.
type issuingAdmin struct {
	noopAdmin
	issued int
}

func (a *issuingAdmin) IssueToken(ns, _ string, _ int) (string, error) {
	a.issued++
	return "issued:" + ns, nil
}

// A minted-but-never-redeemed enrollment must leave no token behind: issuance
// happens only at exchange (audit 2026-08-28, SEC-C-04). Two mints then one
// exchange = exactly one token issued.
func TestEnrollMintIssuesNoTokenUntilRedeemed(t *testing.T) {
	r := New(EnvTokenStore(""))
	admin := &issuingAdmin{}
	r.SetAdmin(admin)
	r.SetAdminSecret("s")

	mint := func() string {
		req := httptest.NewRequest("POST", "/admin/enroll/mint", strings.NewReader(`{"namespace":"alice"}`))
		req.Header.Set("X-Admin-Secret", "s")
		w := httptest.NewRecorder()
		r.handleEnrollMint(w, req)
		if w.Code != 200 {
			t.Fatalf("mint status=%d body=%s", w.Code, w.Body.String())
		}
		var out struct{ Code string }
		json.Unmarshal(w.Body.Bytes(), &out)
		return out.Code
	}
	code := mint()
	mint() // a second, abandoned enrollment
	if admin.issued != 0 {
		t.Fatalf("minting issued %d tokens; must issue none until redeemed", admin.issued)
	}
	if w := exchange(t, r, code); w.Code != 200 {
		t.Fatalf("exchange status=%d", w.Code)
	}
	if admin.issued != 1 {
		t.Fatalf("issued %d tokens across two mints and one redemption, want 1", admin.issued)
	}
}
