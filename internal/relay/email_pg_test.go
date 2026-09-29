package relay

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

// migrationsThrough is the embedded set cut off after one version, so a test
// can seed data the way an older release left it before the rest applies.
func migrationsThrough(t *testing.T, last string) fs.FS {
	t.Helper()
	out := fstest.MapFS{}
	entries, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range entries {
		if version, _, _ := strings.Cut(strings.TrimPrefix(path, "migrations/"), "_"); version > last {
			continue
		}
		body, err := fs.ReadFile(migrationFiles, path)
		if err != nil {
			t.Fatal(err)
		}
		out[path] = &fstest.MapFile{Data: body}
	}
	return out
}

// Opt in with WANCTL_TEST_POSTGRES, using the existing isolated-schema helper.
// Addresses known before v0.18.0 come across unconfirmed, with the user row's
// address ahead of any application's.
func TestContactEmailMigrationCarriesAddressesUnconfirmed(t *testing.T) {
	db := deviceIDTestDB(t)
	if err := runMigrations(db, migrationsThrough(t, "011")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO users (provider, provider_subject, namespace, name, role, email) VALUES
 ('github','1','owner','Owner','admin','owner@example.com'),
 ('github','2','applicant','Applicant','user',NULL),
 ('github','3','quiet','Quiet','user','');
INSERT INTO access_requests (provider, subject, login, email, status) VALUES
 ('github','1','owner','typed-owner@example.com','approved'),
 ('github','2','applicant','older@example.com','declined'),
 ('github','2','applicant','newer@example.com','approved'),
 ('github','4','stranger','stranger@example.com','pending'),
 ('github','5','silent','','pending');`); err != nil {
		t.Fatal(err)
	}
	if err := runMigrations(db, migrationFiles); err != nil {
		t.Fatal(err)
	}
	p := &PGStore{db: db}
	for subject, want := range map[string]string{
		"1": "owner@example.com", "2": "newer@example.com", "3": "", "4": "stranger@example.com", "5": "",
	} {
		got, err := p.ContactEmail("github", subject)
		if err != nil {
			t.Fatal(err)
		}
		if got.Address != want || got.ConfirmedAt != nil || got.Confirmed() != "" {
			t.Fatalf("subject %s = %#v, want unconfirmed %q", subject, got, want)
		}
	}
	// An unconfirmed carried-over address is not where an approval goes.
	list, err := p.ListAccessRequests()
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range list {
		if request.Email != "" {
			t.Fatalf("request %d email = %q before any confirmation", request.ID, request.Email)
		}
	}
}

func TestContactEmailPostgresLifecycle(t *testing.T) {
	db := deviceIDTestDB(t)
	if err := runMigrations(db, migrationFiles); err != nil {
		t.Fatal(err)
	}
	p := &PGStore{db: db}

	// The resolve path no longer writes an address anywhere.
	if _, _, err := p.ResolveIdentity("github", "1", "owner", "Owner", "portal"); err != nil {
		t.Fatal(err)
	}
	var stored *string
	if err := db.QueryRow(`SELECT email FROM users WHERE provider='github' AND provider_subject='1'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != nil && *stored != "" {
		t.Fatalf("users.email written: %q", *stored)
	}

	sent, token, err := p.IssueEmailConfirmation("github", "2", "applicant", "first@example.com", "/pending")
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 43 || sent.Address != "first@example.com" || sent.Next != "/pending" {
		t.Fatalf("issued = %#v token %q", sent, token)
	}
	var hash []byte
	if err := db.QueryRow(`SELECT token_hash FROM email_confirmations WHERE id=$1`, sent.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if string(hash) == token || len(hash) != 32 {
		t.Fatalf("stored token_hash is not a SHA-256 of the token: %x", hash)
	}
	pending, err := p.ContactEmail("github", "2")
	if err != nil || pending.Pending != "first@example.com" || pending.Confirmed() != "" {
		t.Fatalf("pending = %#v, %v", pending, err)
	}

	// Peeking is what the page's GET does; it must not confirm.
	for range 3 {
		if link, err := p.PeekEmailConfirmation(token); err != nil || link.Address != "first@example.com" || link.Login != "applicant" {
			t.Fatalf("peek = %#v, %v", link, err)
		}
	}
	if c, _ := p.ContactEmail("github", "2"); c.Confirmed() != "" {
		t.Fatalf("peek confirmed: %#v", c)
	}
	if _, err := p.PeekEmailConfirmation("not-a-token"); !errors.Is(err, ErrTokenUnknown) {
		t.Fatalf("unknown peek = %v", err)
	}

	if err := p.MarkEmailConfirmationSent(sent.ID); err != nil {
		t.Fatal(err)
	}
	// A resend supersedes the first link once its mail is out; the first then
	// reads as expired. (Same address inside ten minutes is refused, so the
	// resend is to a corrected address.) A refused mail supersedes nothing.
	if _, _, err := p.IssueEmailConfirmation("github", "2", "applicant", "first@example.com", "/"); !errors.Is(err, ErrEmailRateAddress) {
		t.Fatalf("same address within interval = %v", err)
	}
	refused, _, err := p.IssueEmailConfirmation("github", "2", "applicant", "typo@example.com", "/pending")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.FailEmailConfirmation(refused.ID); err != nil {
		t.Fatal(err)
	}
	if link, err := p.PeekEmailConfirmation(token); err != nil || link.Address != "first@example.com" {
		t.Fatalf("first link after a refused resend = %#v, %v", link, err)
	}
	if c, _ := p.ContactEmail("github", "2"); c.Pending != "first@example.com" {
		t.Fatalf("pending after a refused resend = %#v", c)
	}
	second, token2, err := p.IssueEmailConfirmation("github", "2", "applicant", "second@example.com", "/pending")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.MarkEmailConfirmationSent(second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ConfirmEmail(token); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("superseded confirm = %v", err)
	}
	link, err := p.ConfirmEmail(token2)
	if err != nil || link.Address != "second@example.com" || link.Next != "/pending" || link.ID != second.ID {
		t.Fatalf("confirm = %#v, %v", link, err)
	}
	if _, err := p.ConfirmEmail(token2); !errors.Is(err, ErrTokenUsed) {
		t.Fatalf("reuse = %v", err)
	}
	if _, err := p.PeekEmailConfirmation(token2); !errors.Is(err, ErrTokenUsed) {
		t.Fatalf("peek after use = %v", err)
	}
	confirmed, err := p.ContactEmail("github", "2")
	if err != nil || confirmed.Confirmed() != "second@example.com" || confirmed.Pending != "" {
		t.Fatalf("after confirm = %#v, %v", confirmed, err)
	}
	if _, _, err := p.IssueEmailConfirmation("github", "2", "applicant", "SECOND@example.com", "/"); !errors.Is(err, ErrEmailUnchanged) {
		t.Fatalf("same address again = %v", err)
	}

	// Changing the address keeps the old one in force until the new link is
	// used; an expired link is refused.
	_, token3, err := p.IssueEmailConfirmation("github", "2", "applicant", "third@example.com", "/")
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := p.ContactEmail("github", "2"); c.Confirmed() != "second@example.com" || c.Pending != "third@example.com" {
		t.Fatalf("during change = %#v", c)
	}
	if _, err := db.Exec(`UPDATE email_confirmations SET expires_at = now() - interval '1 second' WHERE address = 'third@example.com'`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ConfirmEmail(token3); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expired confirm = %v", err)
	}
	if c, _ := p.ContactEmail("github", "2"); c.Confirmed() != "second@example.com" || c.Pending != "" {
		t.Fatalf("after expiry = %#v", c)
	}

	// The access queue reads the confirmed address.
	request, err := p.CreateAccessRequest("github", "2", "applicant", "hello")
	if err != nil || request.Email != "second@example.com" {
		t.Fatalf("create = %#v, %v", request, err)
	}
	approved, ok, err := p.DecideAccessRequest(request.ID, accessApproved, "owner")
	if err != nil || !ok || approved.Email != "second@example.com" {
		t.Fatalf("approve = %#v, %v, %v", approved, ok, err)
	}
	latest, ok, err := p.LatestAccessRequest("github", "2")
	if err != nil || !ok || latest.Email != "second@example.com" {
		t.Fatalf("latest = %#v, %v, %v", latest, ok, err)
	}
}

func TestContactEmailPostgresRateLimits(t *testing.T) {
	db := deviceIDTestDB(t)
	if err := runMigrations(db, migrationFiles); err != nil {
		t.Fatal(err)
	}
	p := &PGStore{db: db}

	// A send whose mail was refused leaves the mailbox free at once, but it
	// was a send: it counts toward the day's five.
	failed, _, err := p.IssueEmailConfirmation("github", "9", "busy", "a@example.com", "/")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.FailEmailConfirmation(failed.ID); err != nil {
		t.Fatal(err)
	}
	for i, address := range []string{"a@example.com", "b@example.com", "c@example.com", "d@example.com"} {
		if _, _, err := p.IssueEmailConfirmation("github", "9", "busy", address, "/"); err != nil {
			t.Fatalf("send %d: %v", i+1, err)
		}
	}
	_, _, err = p.IssueEmailConfirmation("github", "9", "busy", "f@example.com", "/")
	var rate *EmailRateError
	if !errors.As(err, &rate) || !errors.Is(err, ErrEmailRateIdentity) {
		t.Fatalf("sixth send = %v", err)
	}
	// The cap is per identity: another one may still send, but not to a
	// mailbox that got a link minutes ago.
	if _, _, err := p.IssueEmailConfirmation("github", "10", "other", "a@example.com", "/"); !errors.Is(err, ErrEmailRateAddress) {
		t.Fatalf("other identity, same mailbox = %v", err)
	}
	if _, _, err := p.IssueEmailConfirmation("github", "10", "other", "g@example.com", "/"); err != nil {
		t.Fatalf("other identity = %v", err)
	}
	// A day later the oldest send ages out and one slot frees.
	if _, err := db.Exec(`UPDATE email_confirmations SET created_at = created_at - interval '25 hours' WHERE id = $1`, failed.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.IssueEmailConfirmation("github", "9", "busy", "f@example.com", "/"); err != nil {
		t.Fatalf("after a day = %v", err)
	}
}
