package relay

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"wanctl/internal/delegation"
	"wanctl/internal/transport"
)

// scopedPGStore is a migrated schema with accounts alice and bob, a full
// token for alice, and one enrolled device each.
func scopedPGStore(t *testing.T) (p *PGStore, full, aliceDevice, bobDevice string) {
	t.Helper()
	p, alice, _ := disableTestStore(t)
	aliceDevice, bobDevice = uuid.NewString(), uuid.NewString()
	for ns, id := range map[string]string{"alice": aliceDevice, "bob": bobDevice} {
		if _, err := p.RegisterDevice(ns, id, ns+" device", transport.Fingerprint([]byte("fp-"+id))); err != nil {
			t.Fatal(err)
		}
	}
	return p, alice, aliceDevice, bobDevice
}

func TestScopedTokenPostgresLifecycle(t *testing.T) {
	p, full, device, bobDevice := scopedPGStore(t)
	raw, info, err := p.IssueScopedToken("alice", device, "systemctl restart app", "ci", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, delegation.ScopedTokenPrefix) || info.Device != device || info.Command != "systemctl restart app" {
		t.Fatalf("issue: %q %+v", raw, info)
	}
	if d := time.Until(info.ExpiresAt); d < 89*24*time.Hour || d > 91*24*time.Hour {
		t.Fatalf("default lifetime: %v", d)
	}
	// Stored as a hash, never the token.
	var n int
	if err := p.db.QueryRow(`SELECT count(*) FROM tokens WHERE hash=$1 AND kind='scoped'`, HashToken(raw)).Scan(&n); err != nil || n != 1 {
		t.Fatalf("hash row: %d %v", n, err)
	}
	if err := p.db.QueryRow(`SELECT count(*) FROM tokens WHERE hash=$1`, raw).Scan(&n); err != nil || n != 0 {
		t.Fatalf("raw token stored: %d %v", n, err)
	}

	a, ok := p.ResolveAccess(raw)
	if !ok || !a.Delegated || a.ExecOnly != "systemctl restart app" || a.Namespace != "alice" || len(a.Devices) != 1 ||
		a.Devices[0].ID != device || a.GrantID == "" || a.ExpiresAt.IsZero() || a.ControllerFingerprint != "" {
		t.Fatalf("resolve: %+v %v", a, ok)
	}
	if _, ok := ResolveAccess(p, raw); !ok {
		t.Fatal("relay-level invariants rejected a good token")
	}
	if !a.Allows("alice/"+device) || a.Allows("alice/"+bobDevice) || a.Allows("bob/"+bobDevice) {
		t.Fatal("scope covers the wrong devices")
	}
	// The namespace-only paths must not turn it into alice's own authority.
	if ns, ok := p.Resolve(raw); ok {
		t.Fatalf("legacy Resolve accepted a scoped token as %q", ns)
	}
	if p.TokenAccountDisabled(raw) {
		t.Fatal("scoped token reported as an account token")
	}
	// Listing shows it, without the secret.
	list, err := p.ListScopedTokens("alice")
	if err != nil || len(list) != 1 || list[0].ID != info.ID || list[0].RevokedAt != nil {
		t.Fatalf("list: %+v %v", list, err)
	}
	if other, _ := p.ListScopedTokens("bob"); len(other) != 0 {
		t.Fatalf("bob sees alice's tokens: %+v", other)
	}
	// The portal's generic token list shows it too, as its own kind.
	all, err := p.ListTokens("alice")
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, row := range all {
		kinds = append(kinds, row["kind"].(string))
		if row["kind"] == "scoped" && (row["scoped_command"] != "systemctl restart app" || row["scoped_device"] != device) {
			t.Fatalf("portal token list lost the scope: %+v", row)
		}
	}
	if !strings.Contains(strings.Join(kinds, ","), "scoped") {
		t.Fatalf("portal token list lost the scoped token: %v", kinds)
	}
	// The full token is unaffected.
	if ns, ok := p.Resolve(full); !ok || ns != "alice" {
		t.Fatal("full token stopped resolving")
	}
	if fa, ok := p.ResolveAccess(full); !ok || fa.Delegated || fa.ExecOnly != "" {
		t.Fatalf("full token access: %+v", fa)
	}

	// Revocation: only the owner, only once, effective immediately.
	if revoked, err := p.RevokeScopedToken("bob", info.ID); err != nil || revoked {
		t.Fatalf("bob revoked alice's token: %v %v", revoked, err)
	}
	if _, ok := p.ResolveAccess(raw); !ok {
		t.Fatal("a refused revoke still revoked")
	}
	if revoked, err := p.RevokeScopedToken("alice", info.ID); err != nil || !revoked {
		t.Fatalf("revoke: %v %v", revoked, err)
	}
	if _, ok := p.ResolveAccess(raw); ok {
		t.Fatal("revoked token still resolves")
	}
	if revoked, _ := p.RevokeScopedToken("alice", info.ID); revoked {
		t.Fatal("second revoke reported success")
	}
	// The scoped revoke cannot be pointed at a full token.
	var fullID int
	if err := p.db.QueryRow(`SELECT id FROM tokens WHERE hash=$1`, HashToken(full)).Scan(&fullID); err != nil {
		t.Fatal(err)
	}
	if revoked, _ := p.RevokeScopedToken("alice", fullID); revoked {
		t.Fatal("scoped revoke revoked a full token")
	}
	if _, ok := p.Resolve(full); !ok {
		t.Fatal("full token was revoked through the scoped endpoint")
	}
}

func TestScopedTokenPostgresExpiryDeviceIdentityAndAccount(t *testing.T) {
	p, _, device, _ := scopedPGStore(t)
	issue := func() (string, ScopedToken) {
		t.Helper()
		raw, info, err := p.IssueScopedToken("alice", device, "uptime", "", 1)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := p.ResolveAccess(raw); !ok {
			t.Fatal("fresh token does not resolve")
		}
		return raw, info
	}

	raw, info := issue()
	if _, err := p.db.Exec(`UPDATE tokens SET expires_at = now() - interval '1 second' WHERE id=$1`, info.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.ResolveAccess(raw); ok {
		t.Fatal("expired token still resolves")
	}

	// The device re-enrolled under a new identity: the token does not follow.
	raw, _ = issue()
	if _, err := p.db.Exec(`UPDATE devices SET fingerprint='SHA256:replaced' WHERE owner_namespace='alice' AND device_id=$1`, device); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.ResolveAccess(raw); ok {
		t.Fatal("token followed a replaced device identity")
	}
	if _, err := p.db.Exec(`UPDATE devices SET fingerprint=$2 WHERE owner_namespace='alice' AND device_id=$1`, device, transport.Fingerprint([]byte("fp-"+device))); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.ResolveAccess(raw); !ok {
		t.Fatal("token did not resume with the original identity")
	}
	// The device removed.
	if _, err := p.db.Exec(`DELETE FROM devices WHERE owner_namespace='alice' AND device_id=$1`, device); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.ResolveAccess(raw); ok {
		t.Fatal("token resolves for a deleted device")
	}

	// A disabled account's tokens do not resolve; enabling restores them.
	p2, _, device2, _ := scopedPGStore(t)
	raw2, _, err := p2.IssueScopedToken("alice", device2, "uptime", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p2.SetAccountDisabled("alice", true); err != nil {
		t.Fatal(err)
	}
	if _, ok := p2.ResolveAccess(raw2); ok {
		t.Fatal("token of a disabled account resolves")
	}
	if _, _, err := p2.SetAccountDisabled("alice", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := p2.ResolveAccess(raw2); !ok {
		t.Fatal("token did not come back with the account")
	}
}

func TestScopedTokenPostgresRefusesBadIssues(t *testing.T) {
	p, _, device, bobDevice := scopedPGStore(t)
	tooLong := strings.Repeat("a", scopedMaxCommand+1)
	for name, tc := range map[string]struct {
		device, command, label string
		days                   int
		want                   error
	}{
		"empty command":     {device, "", "", 1, ErrScopedInvalid},
		"newline":           {device, "echo a\necho b", "", 1, ErrScopedInvalid},
		"nul":               {device, "echo a\x00b", "", 1, ErrScopedInvalid},
		"escape":            {device, "echo \x1b[31m", "", 1, ErrScopedInvalid},
		"too long":          {device, tooLong, "", 1, ErrScopedInvalid},
		"bad utf8":          {device, "echo \xff", "", 1, ErrScopedInvalid},
		"days negative":     {device, "uptime", "", -1, ErrScopedInvalid},
		"days over max":     {device, "uptime", "", ScopedMaxDays + 1, ErrScopedInvalid},
		"label control":     {device, "uptime", "a\nb", 1, ErrScopedInvalid},
		"label too long":    {device, "uptime", strings.Repeat("x", scopedMaxLabel+1), 1, ErrScopedInvalid},
		"someone's device":  {bobDevice, "uptime", "", 1, ErrDeviceNotFound},
		"unknown device":    {uuid.NewString(), "uptime", "", 1, ErrDeviceNotFound},
		"by someone's name": {"bob device", "uptime", "", 1, ErrDeviceNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			if raw, _, err := p.IssueScopedToken("alice", tc.device, tc.command, tc.label, tc.days); !errors.Is(err, tc.want) || raw != "" {
				t.Fatalf("raw=%q err=%v want %v", raw, err, tc.want)
			}
		})
	}
	// A tab is fine, a long-but-legal command at the limit is fine, name lookup works.
	for _, ok := range []string{"printf 'a\tb'", strings.Repeat("a", scopedMaxCommand)} {
		if _, _, err := p.IssueScopedToken("alice", "alice device", ok, "", 365); err != nil {
			t.Fatalf("%.20q: %v", ok, err)
		}
	}
	if n := countRows(t, p, `SELECT count(*) FROM tokens WHERE kind='scoped'`); n != 2 {
		t.Fatalf("refused issues left rows behind: %d scoped tokens", n)
	}
	// A device with no stable id (legacy registration) cannot be scoped.
	if _, err := p.db.Exec(`INSERT INTO devices(owner_namespace,device_id,display_name,fingerprint) VALUES ('alice','old-name','old-name','SHA256:x')`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.IssueScopedToken("alice", "old-name", "uptime", "", 1); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("legacy device: %v", err)
	}
}

func TestScopedTokenPostgresLiveCap(t *testing.T) {
	p, _, device, _ := scopedPGStore(t)
	var first int
	for i := 0; i < scopedMaxLive; i++ {
		_, info, err := p.IssueScopedToken("alice", device, "uptime", "", 1)
		if err != nil {
			t.Fatalf("token %d: %v", i, err)
		}
		if i == 0 {
			first = info.ID
		}
	}
	if _, _, err := p.IssueScopedToken("alice", device, "uptime", "", 1); !errors.Is(err, ErrScopedLimit) {
		t.Fatalf("51st token: %v", err)
	}
	if revoked, _ := p.RevokeScopedToken("alice", first); !revoked {
		t.Fatal("revoke")
	}
	if _, _, err := p.IssueScopedToken("alice", device, "uptime", "", 1); err != nil {
		t.Fatalf("after revoke: %v", err)
	}
}

func countRows(t *testing.T, p *PGStore, query string) int {
	t.Helper()
	var n int
	if err := p.db.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
