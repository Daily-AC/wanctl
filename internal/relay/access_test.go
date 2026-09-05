package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wanctl/internal/notify"
)

// accessAdmin is a whole access-request store in memory, plus the invites it
// writes on approval — approval is not a second admission mechanism, and the
// only way to assert that is to watch which invite row it produces.
type accessAdmin struct {
	// memNotifyAdmin, not noopAdmin: SetAdmin reads the webhook store off the
	// same object by type assertion, so an admin store that cannot hold a
	// webhook can never deliver one.
	memNotifyAdmin
	rows    []AccessRequest
	invites []string // github_login per invite created by an approval
	admins  []string
	nextID  int
	now     time.Time
}

func (a *accessAdmin) at() time.Time {
	if a.now.IsZero() {
		return time.Now().UTC()
	}
	return a.now
}

func (a *accessAdmin) latest(provider, subject string) (AccessRequest, bool) {
	for i := len(a.rows) - 1; i >= 0; i-- {
		if a.rows[i].Provider == provider && a.rows[i].Subject == subject {
			return a.rows[i], true
		}
	}
	return AccessRequest{}, false
}

func (a *accessAdmin) CreateAccessRequest(provider, subject, login, note string) (AccessRequest, error) {
	latest, found := a.latest(provider, subject)
	if err := accessRequestGate(latest, found, a.at()); err != nil {
		return AccessRequest{}, err
	}
	a.nextID++
	row := AccessRequest{
		ID: a.nextID, Provider: provider, Subject: subject, Login: login,
		Note: note, Status: accessPending, CreatedAt: a.at(),
	}
	a.rows = append(a.rows, row)
	return row, nil
}

func (a *accessAdmin) LatestAccessRequest(provider, subject string) (AccessRequest, bool, error) {
	row, found := a.latest(provider, subject)
	return row, found, nil
}

func (a *accessAdmin) ListAccessRequests() ([]AccessRequest, error) { return a.rows, nil }

func (a *accessAdmin) DecideAccessRequest(id int, status, decidedBy string) (AccessRequest, bool, error) {
	for i := range a.rows {
		if a.rows[i].ID != id || a.rows[i].Status != accessPending {
			continue
		}
		at := a.at()
		a.rows[i].Status = status
		a.rows[i].DecidedAt = &at
		a.rows[i].DecidedBy = decidedBy
		if status == accessApproved {
			a.invites = append(a.invites, a.rows[i].Login)
		}
		return a.rows[i], true, nil
	}
	return AccessRequest{}, false, nil
}

func (a *accessAdmin) ListAdminNamespaces() ([]string, error) { return a.admins, nil }

func accessRelay(t *testing.T, admin *accessAdmin) *Relay {
	t.Helper()
	r := New(envTokens{})
	r.SetAdminSecret("s3cret")
	r.SetAdmin(admin)
	return r
}

func accessDo(t *testing.T, r *Relay, method, path, body, secret string) *httptest.ResponseRecorder {
	t.Helper()
	return relayRequest(t, r, method, path, body, "", secret)
}

// The gate is one rule read by two callers — the submit path and the form the
// applicant is shown. Its whole contract is in this table.
func TestAccessRequestGate(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	decided := func(status string, ago time.Duration) AccessRequest {
		at := now.Add(-ago)
		return AccessRequest{Status: status, DecidedAt: &at}
	}
	cases := []struct {
		name   string
		latest AccessRequest
		found  bool
		want   error
	}{
		{"never asked", AccessRequest{}, false, nil},
		{"one already waiting", AccessRequest{Status: accessPending}, true, ErrAccessRequestOpen},
		{"already approved", decided(accessApproved, time.Hour), true, ErrAccessRequestSettled},
		{"declined yesterday", decided(accessDeclined, 24*time.Hour), true, ErrAccessRequestCooldown},
		{"declined 6 days ago", decided(accessDeclined, 6*24*time.Hour), true, ErrAccessRequestCooldown},
		{"declined 7 days ago", decided(accessDeclined, 7*24*time.Hour), true, nil},
		{"declined 8 days ago", decided(accessDeclined, 8*24*time.Hour), true, nil},
	}
	for _, c := range cases {
		if got := accessRequestGate(c.latest, c.found, now); got != c.want {
			t.Errorf("%s: gate = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAccessRequestCreateDedupeAndReapply(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	admin := &accessAdmin{now: now}
	r := accessRelay(t, admin)
	body := `{"provider":"github","subject":"8437","login":"octocat","note":"  hi   there  "}`

	rr := accessDo(t, r, "POST", "/admin/access-requests", body, "s3cret")
	if rr.Code != http.StatusOK {
		t.Fatalf("first request: %d %q", rr.Code, rr.Body.String())
	}
	var first AccessRequest
	json.Unmarshal(rr.Body.Bytes(), &first)
	if first.Status != accessPending || first.Note != "hi there" {
		t.Fatalf("first request = %#v (note should be collapsed to plain text)", first)
	}

	// Second click, same account: the one-open-request rule is the dedupe.
	if rr := accessDo(t, r, "POST", "/admin/access-requests", body, "s3cret"); rr.Code != http.StatusConflict ||
		rr.Body.String() != "request-open" {
		t.Fatalf("second request = %d %q, want 409 request-open", rr.Code, rr.Body.String())
	}

	// Declined, then too soon.
	if rr := accessDo(t, r, "POST", "/admin/access-requests/decide",
		`{"id":1,"decision":"declined","decided_by":"alice"}`, "s3cret"); rr.Code != http.StatusOK {
		t.Fatalf("decline: %d %q", rr.Code, rr.Body.String())
	}
	admin.now = now.Add(6 * 24 * time.Hour)
	if rr := accessDo(t, r, "POST", "/admin/access-requests", body, "s3cret"); rr.Code != http.StatusTooManyRequests ||
		rr.Body.String() != "request-cooldown" {
		t.Fatalf("re-apply after 6 days = %d %q, want 429 request-cooldown", rr.Code, rr.Body.String())
	}

	// Seven days later it is allowed again.
	admin.now = now.Add(7 * 24 * time.Hour)
	if rr := accessDo(t, r, "POST", "/admin/access-requests", body, "s3cret"); rr.Code != http.StatusOK {
		t.Fatalf("re-apply after 7 days = %d %q, want 200", rr.Code, rr.Body.String())
	}
	if len(admin.rows) != 2 {
		t.Fatalf("rows = %#v, want two applications", admin.rows)
	}

	// An approved account never sees the form again.
	if rr := accessDo(t, r, "POST", "/admin/access-requests/decide",
		`{"id":2,"decision":"approved","decided_by":"alice"}`, "s3cret"); rr.Code != http.StatusOK {
		t.Fatalf("approve: %d %q", rr.Code, rr.Body.String())
	}
	admin.now = now.Add(90 * 24 * time.Hour)
	if rr := accessDo(t, r, "POST", "/admin/access-requests", body, "s3cret"); rr.Code != http.StatusConflict ||
		rr.Body.String() != "request-approved" {
		t.Fatalf("apply after approval = %d %q, want 409 request-approved", rr.Code, rr.Body.String())
	}
}

// Approving must produce the invite the existing admission path already knows
// how to redeem, bound to the applicant's login — not a second mechanism.
func TestAccessRequestApproveIssuesInviteForThatLogin(t *testing.T) {
	admin := &accessAdmin{}
	r := accessRelay(t, admin)
	accessDo(t, r, "POST", "/admin/access-requests",
		`{"provider":"github","subject":"8437","login":"octocat"}`, "s3cret")

	rr := accessDo(t, r, "POST", "/admin/access-requests/decide",
		`{"id":1,"decision":"approved","decided_by":"alice"}`, "s3cret")
	if rr.Code != http.StatusOK {
		t.Fatalf("approve: %d %q", rr.Code, rr.Body.String())
	}
	var out AccessRequest
	json.Unmarshal(rr.Body.Bytes(), &out)
	if out.Status != accessApproved || out.DecidedBy != "alice" || out.DecidedAt == nil {
		t.Fatalf("approved row = %#v", out)
	}
	if len(admin.invites) != 1 || admin.invites[0] != "octocat" {
		t.Fatalf("invites created = %#v, want one bound to octocat", admin.invites)
	}
	// A decision is made once.
	if rr := accessDo(t, r, "POST", "/admin/access-requests/decide",
		`{"id":1,"decision":"declined","decided_by":"alice"}`, "s3cret"); rr.Code != http.StatusNotFound {
		t.Fatalf("second decision = %d %q, want 404", rr.Code, rr.Body.String())
	}
}

// The applicant's own view: their state and the gate's verdict, nothing else.
func TestAccessRequestStatusIsScopedToOneSubject(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	admin := &accessAdmin{now: now}
	r := accessRelay(t, admin)
	accessDo(t, r, "POST", "/admin/access-requests",
		`{"provider":"github","subject":"8437","login":"octocat","note":"let me in"}`, "s3cret")

	rr := accessDo(t, r, "GET", "/admin/access-requests/status?provider=github&subject=8437", "", "s3cret")
	var mine map[string]any
	json.Unmarshal(rr.Body.Bytes(), &mine)
	if mine["status"] != "pending" || mine["can_apply"] != false || mine["note"] != "let me in" {
		t.Fatalf("own status = %#v", mine)
	}
	rr = accessDo(t, r, "GET", "/admin/access-requests/status?provider=github&subject=999", "", "s3cret")
	var stranger map[string]any
	json.Unmarshal(rr.Body.Bytes(), &stranger)
	if stranger["status"] != "none" || stranger["can_apply"] != true {
		t.Fatalf("stranger status = %#v", stranger)
	}
	if _, leaked := stranger["note"]; leaked {
		t.Fatalf("status leaked another account's note: %#v", stranger)
	}
}

// Every leg is behind the admin secret: the relay is public, and the queue is
// private.
func TestAccessRequestEndpointsRequireAdminSecret(t *testing.T) {
	admin := &accessAdmin{}
	r := accessRelay(t, admin)
	for _, c := range [][3]string{
		{"GET", "/admin/access-requests", ""},
		{"POST", "/admin/access-requests", `{"provider":"github","subject":"1","login":"x"}`},
		{"GET", "/admin/access-requests/status?provider=github&subject=1", ""},
		{"POST", "/admin/access-requests/decide", `{"id":1,"decision":"approved"}`},
	} {
		if rr := accessDo(t, r, c[0], c[1], c[2], ""); rr.Code != http.StatusForbidden {
			t.Fatalf("%s %s without the secret = %d, want 403", c[0], c[1], rr.Code)
		}
		if rr := accessDo(t, r, c[0], c[1], c[2], "wrong"); rr.Code != http.StatusForbidden {
			t.Fatalf("%s %s with a wrong secret = %d, want 403", c[0], c[1], rr.Code)
		}
	}
	if len(admin.rows) != 0 {
		t.Fatalf("store was reached without the secret: %#v", admin.rows)
	}
}

func TestAccessRequestNoteIsPlainTextAndBounded(t *testing.T) {
	long := strings.Repeat("x", accessNoteMax+40)
	if got := normalizeAccessNote(long); len([]rune(got)) != accessNoteMax {
		t.Fatalf("note length = %d, want %d", len([]rune(got)), accessNoteMax)
	}
	if got := normalizeAccessNote("  hello\n\nthere\tfriend  "); got != "hello there friend" {
		t.Fatalf("note = %q", got)
	}
	if got := normalizeAccessNote(strings.Repeat("字", accessNoteMax+5)); len([]rune(got)) != accessNoteMax {
		t.Fatalf("CJK note was cut by bytes, not characters: %d runes", len([]rune(got)))
	}
}

// A new application is an account-level event, like a friend request: it goes
// to the administrators, over the webhook path that already exists.
func TestAccessRequestNotifiesAdmins(t *testing.T) {
	admin := &accessAdmin{admins: []string{"alice"}}
	// Delivery is a category-gated decision: this is security-shaped, like a
	// friend request, so a webhook with OnSecurity off must stay quiet.
	admin.webhooks = map[string]NotifyWebhook{"alice": {
		Namespace: "alice", URL: "https://hooks.example/topic",
		Format: notify.FormatJSON, OnSecurity: true,
	}}
	sender := &accessEventSender{events: make(chan notify.Event, 4)}
	r := accessRelay(t, admin)
	r.SetNotifySender(sender)

	rr := accessDo(t, r, "POST", "/admin/access-requests",
		`{"provider":"github","subject":"8437","login":"octocat","note":"hi"}`, "s3cret")
	if rr.Code != http.StatusOK {
		t.Fatalf("create: %d %q", rr.Code, rr.Body.String())
	}
	select {
	case got := <-sender.events:
		if got.Event != "access.requested" || got.Namespace != "alice" ||
			got.Peer != "octocat" || got.Detail != "hi" {
			t.Fatalf("event = %#v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no webhook was delivered for a new access request")
	}
}

type accessEventSender struct{ events chan notify.Event }

func (s *accessEventSender) Send(_ context.Context, _ notify.Destination, event notify.Event) (notify.Result, error) {
	s.events <- event
	return notify.Result{HTTPStatus: 200, Attempts: 1, Event: event.Event}, nil
}
