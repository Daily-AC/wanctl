package main

import (
	"testing"

	"wanctl/internal/client"
	"wanctl/internal/config"
	"wanctl/internal/transport"
)

// Enrolling seeds the portal identity that came back with the token, so a fresh
// device accepts the team portal's web console without anyone remembering
// --portal-fps. The agent fails closed on an empty admin set, so skipping this
// is indistinguishable from the portal being broken.
func TestSeedPortalAdminTrustsEnrolledPortal(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	fp := transport.Fingerprint([]byte("portal cert"))

	seedPortalAdmin(fp)

	admins, err := config.OpenPortalAdmins()
	if err != nil {
		t.Fatal(err)
	}
	if !admins.Contains(fp) {
		t.Fatalf("portal admin not seeded; have %v", admins.List())
	}
}

// Enrollments that carry no fingerprint (an older portal, or a relay that never
// got one) must leave the set exactly as it was rather than writing an empty
// entry.
func TestSeedPortalAdminIgnoresEmpty(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())

	seedPortalAdmin("")

	admins, err := config.OpenPortalAdmins()
	if err != nil {
		t.Fatal(err)
	}
	if len(admins.List()) != 0 {
		t.Fatalf("empty fingerprint seeded something: %v", admins.List())
	}
}

// Re-enrolling must not disturb an existing set: --portal-fps and any earlier
// enrollment stay trusted alongside.
func TestSeedPortalAdminIsAdditive(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	existing := transport.Fingerprint([]byte("hand-passed portal"))
	fresh := transport.Fingerprint([]byte("enrolled portal"))
	admins, err := config.OpenPortalAdmins()
	if err != nil {
		t.Fatal(err)
	}
	if err := admins.Add(existing); err != nil {
		t.Fatal(err)
	}

	seedPortalAdmin(fresh)

	reopened, err := config.OpenPortalAdmins()
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Contains(existing) || !reopened.Contains(fresh) {
		t.Fatalf("want both trusted, have %v", reopened.List())
	}
}

// The seam that actually matters: completing an enrollment must both hand back
// the token and trust the portal that came with it.
func TestApplyEnrollmentSeedsPortalAdmin(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	fp := transport.Fingerprint([]byte("portal cert"))

	token := applyEnrollment(client.Enrollment{Token: "wanctl_tok", Namespace: "alice", PortalFP: fp})

	if token != "wanctl_tok" {
		t.Fatalf("token = %q", token)
	}
	admins, err := config.OpenPortalAdmins()
	if err != nil {
		t.Fatal(err)
	}
	if !admins.Contains(fp) {
		t.Fatalf("enrollment did not trust its portal; have %v", admins.List())
	}
}
