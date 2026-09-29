package relay

import "testing"

// Opt in with WANCTL_TEST_POSTGRES, using the existing isolated-schema helper.
func TestEmailPostgresPersistence(t *testing.T) {
	db := deviceIDTestDB(t)
	if err := runMigrations(db, migrationFiles); err != nil {
		t.Fatal(err)
	}
	p := &PGStore{db: db}
	if _, _, err := p.ResolveIdentity("github", "1", "owner", "Owner", "", "portal", "owner@example.com"); err != nil {
		t.Fatal(err)
	}
	request, err := p.CreateAccessRequest("github", "2", "applicant", "hello", "typed@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if request.Email != "typed@example.com" {
		t.Fatalf("created email = %q", request.Email)
	}
	approved, ok, err := p.DecideAccessRequest(request.ID, accessApproved, "owner")
	if err != nil || !ok || approved.Email != request.Email {
		t.Fatalf("approve = %#v, %v, %v", approved, ok, err)
	}
	latest, ok, err := p.LatestAccessRequest("github", "2")
	if err != nil || !ok || latest.Email != request.Email {
		t.Fatalf("latest = %#v, %v, %v", latest, ok, err)
	}
	list, err := p.ListAccessRequests()
	if err != nil || len(list) != 1 || list[0].Email != request.Email {
		t.Fatalf("list = %#v, %v", list, err)
	}
	// Other accounts and later declined requests must not supply the fallback.
	if _, err := db.Exec(`INSERT INTO access_requests(provider,subject,login,email,status) VALUES
 ('github','2','applicant','declined@example.com','declined'),
 ('github','3','other','other@example.com','approved')`); err != nil {
		t.Fatal(err)
	}
	resolve := func(email, want string) {
		t.Helper()
		if _, _, err := p.ResolveIdentity("github", "2", "applicant", "", "", "portal", email); err != nil {
			t.Fatal(err)
		}
		var stored string
		if err := db.QueryRow(`SELECT email FROM users WHERE provider='github' AND provider_subject='2'`).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored != want {
			t.Fatalf("stored = %q, want %q", stored, want)
		}
	}
	resolve("", "typed@example.com")
	resolve("primary@example.com", "primary@example.com")
	resolve("", "primary@example.com")
}
