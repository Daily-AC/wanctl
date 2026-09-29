package relay

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestContactAddressValidation(t *testing.T) {
	for _, tc := range []struct {
		name, email string
		valid       bool
	}{
		{"valid", "person@example.com", true},
		{"trim", " person@example.com ", true},
		{"missing", "", false},
		{"display name", "Person <person@example.com>", false},
		{"noreply", "123@users.noreply.github.com", false},
		{"noreply case", "123@USERS.NOREPLY.GITHUB.COM", false},
		{"255 bytes", strings.Repeat("x", 243) + "@example.com", false},
		{"no dot", "person@localhost", false},
		{"header injection", "person@example.com\r\nBcc: other@example.com", false},
		{"control", "person\x00@example.com", false},
		{"trailing control", "person@example.com\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := contactAddress(tc.email)
			if tc.valid != (err == nil) || tc.valid && got != strings.TrimSpace(tc.email) {
				t.Fatalf("contactAddress(%q) = %q, %v", tc.email, got, err)
			}
		})
	}
}

// contactAdmin answers the contact endpoints with whatever the test sets.
type contactAdmin struct {
	noopAdmin
	issueErr   error
	confirmErr error
	peekErr    error
	issued     []string
	dropped    []int
}

func (a *contactAdmin) IssueEmailConfirmation(provider, subject, login, address, next string) (EmailConfirmation, string, error) {
	if a.issueErr != nil {
		return EmailConfirmation{}, "", a.issueErr
	}
	a.issued = append(a.issued, provider+"/"+subject+" "+address+" "+next)
	return EmailConfirmation{ID: 7, Address: address, ExpiresAt: time.Now().Add(emailTokenTTL)}, "raw-token", nil
}

func (a *contactAdmin) DropEmailConfirmation(id int) error {
	a.dropped = append(a.dropped, id)
	return nil
}

func (a *contactAdmin) PeekEmailConfirmation(string) (EmailConfirmation, error) {
	return EmailConfirmation{Login: "octocat", Address: "person@example.com"}, a.peekErr
}

func (a *contactAdmin) ConfirmEmail(string) (EmailConfirmation, error) {
	if a.confirmErr != nil {
		return EmailConfirmation{}, a.confirmErr
	}
	return EmailConfirmation{Provider: "github", Subject: "7", Login: "octocat", Address: "person@example.com", Next: "/pending"}, nil
}

func contactRelay(admin *contactAdmin) *Relay {
	r := New(envTokens{})
	r.SetAdminSecret("s3cret")
	r.SetAdmin(admin)
	return r
}

func TestContactEmailSendEndpoint(t *testing.T) {
	admin := &contactAdmin{}
	r := contactRelay(admin)
	rr := relayRequest(t, r, "POST", "/admin/contact-email/send",
		`{"provider":"GitHub","subject":" 7 ","login":"octocat","address":" person@example.com ","next":"/pending"}`, "", "s3cret")
	var out map[string]any
	json.Unmarshal(rr.Body.Bytes(), &out)
	if rr.Code != 200 || out["token"] != "raw-token" || out["id"] != float64(7) {
		t.Fatalf("send = %d %s", rr.Code, rr.Body.String())
	}
	if len(admin.issued) != 1 || admin.issued[0] != "github/7 person@example.com /pending" {
		t.Fatalf("issued = %#v", admin.issued)
	}

	rr = relayRequest(t, r, "POST", "/admin/contact-email/send",
		`{"provider":"github","subject":"7","address":"Person <person@example.com>"}`, "", "s3cret")
	if rr.Code != 400 || rr.Body.String() != "email-invalid" {
		t.Fatalf("invalid = %d %q", rr.Code, rr.Body.String())
	}

	// A rate refusal says which limit and when it lifts.
	admin.issueErr = &EmailRateError{Kind: ErrEmailRateAddress, RetryAt: time.Now().Add(9 * time.Minute)}
	rr = relayRequest(t, r, "POST", "/admin/contact-email/send",
		`{"provider":"github","subject":"7","address":"person@example.com"}`, "", "s3cret")
	secs, _ := strconv.Atoi(rr.Header().Get("Retry-After"))
	if rr.Code != http.StatusTooManyRequests || rr.Body.String() != "rate-address" || secs < 530 || secs > 541 {
		t.Fatalf("rate = %d %q Retry-After %q", rr.Code, rr.Body.String(), rr.Header().Get("Retry-After"))
	}
	admin.issueErr = ErrEmailUnchanged
	rr = relayRequest(t, r, "POST", "/admin/contact-email/send",
		`{"provider":"github","subject":"7","address":"person@example.com"}`, "", "s3cret")
	if rr.Code != http.StatusConflict || rr.Body.String() != "email-unchanged" {
		t.Fatalf("unchanged = %d %q", rr.Code, rr.Body.String())
	}

	rr = relayRequest(t, r, "POST", "/admin/contact-email/unsend", `{"id":7}`, "", "s3cret")
	if rr.Code != http.StatusNoContent || len(admin.dropped) != 1 || admin.dropped[0] != 7 {
		t.Fatalf("unsend = %d, dropped %v", rr.Code, admin.dropped)
	}
	// Everything here sits behind the admin secret.
	for _, path := range []string{"/admin/contact-email/send", "/admin/contact-email/confirm", "/admin/contact-email/peek"} {
		if rr := relayRequest(t, r, "POST", path, `{"token":"x"}`, "", "wrong"); rr.Code != http.StatusForbidden {
			t.Fatalf("%s without secret = %d", path, rr.Code)
		}
	}
}

func TestContactEmailConfirmEndpoints(t *testing.T) {
	admin := &contactAdmin{}
	r := contactRelay(admin)
	rr := relayRequest(t, r, "POST", "/admin/contact-email/peek", `{"token":"raw-token"}`, "", "s3cret")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"state":"live"`) {
		t.Fatalf("peek = %d %s", rr.Code, rr.Body.String())
	}
	admin.peekErr = ErrTokenUsed
	rr = relayRequest(t, r, "POST", "/admin/contact-email/peek", `{"token":"raw-token"}`, "", "s3cret")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"state":"used"`) {
		t.Fatalf("peek used = %d %s", rr.Code, rr.Body.String())
	}
	admin.peekErr = ErrTokenUnknown
	rr = relayRequest(t, r, "POST", "/admin/contact-email/peek", `{"token":"raw-token"}`, "", "s3cret")
	if rr.Code != 404 || rr.Body.String() != "token-unknown" {
		t.Fatalf("peek unknown = %d %s", rr.Code, rr.Body.String())
	}

	rr = relayRequest(t, r, "POST", "/admin/contact-email/confirm", `{"token":"raw-token"}`, "", "s3cret")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"next":"/pending"`) {
		t.Fatalf("confirm = %d %s", rr.Code, rr.Body.String())
	}
	for err, want := range map[error]int{ErrTokenUsed: 410, ErrTokenExpired: 410, ErrTokenUnknown: 404} {
		admin.confirmErr = err
		rr = relayRequest(t, r, "POST", "/admin/contact-email/confirm", `{"token":"raw-token"}`, "", "s3cret")
		if rr.Code != want || rr.Body.String() != err.Error() {
			t.Fatalf("confirm %v = %d %q", err, rr.Code, rr.Body.String())
		}
	}
	if rr := relayRequest(t, r, "GET", "/admin/contact-email/confirm", "", "", "s3cret"); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET confirm = %d", rr.Code)
	}
}
