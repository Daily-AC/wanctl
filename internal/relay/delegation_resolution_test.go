package relay

import (
	"net/http"
	"strings"
	"testing"

	"wanctl/internal/delegation"
)

func TestDelegatedResolutionDoesNotDiscloseOtherSharedDevices(t *testing.T) {
	grant := testTransportGrant()
	r := New(&transportGrantStore{grants: map[string]delegation.Access{
		"delegate": grant,
		"owner":    {Namespace: "alice", CredentialID: "owner"},
	}})
	store := &memSharedAdmin{devices: []sharedTestDevice{
		{owner: "alice", id: "allowed", label: "My Mac"},
		{owner: "bob", id: "bob-private-device-id", label: "build", grantee: "alice", perms: "exec"},
		{owner: "carol", id: "carol-private-device-id", label: "build", grantee: "alice", perms: "exec"},
	}}
	r.SetAdmin(store)
	r.SetACL(store)

	for _, endpoint := range []string{"/resolve", "/dial", "/h/dial"} {
		t.Run(endpoint, func(t *testing.T) {
			got := grantRequest(r.Handler(), http.MethodGet, endpoint+"?target=build", "delegate")
			wantStatus, wantBody := http.StatusForbidden, "forbidden"
			if endpoint == "/resolve" {
				wantStatus, wantBody = http.StatusConflict, "device unavailable or ambiguous; use its device ID"
			}
			if got.Code != wantStatus || strings.TrimSpace(got.Body.String()) != wantBody {
				t.Fatalf("delegated diagnostic = %d %q; want %d %q", got.Code, got.Body.String(), wantStatus, wantBody)
			}

			// Full account credentials retain actionable ambiguity diagnostics.
			full := grantRequest(r.Handler(), http.MethodGet, endpoint+"?target=build", "owner")
			if full.Code != wantStatus {
				t.Fatalf("owner status = %d want %d", full.Code, wantStatus)
			}
			for _, target := range []string{"bob/bob-private-device-id", "carol/carol-private-device-id"} {
				if !strings.Contains(full.Body.String(), target) {
					t.Fatalf("owner diagnostic lost %q: %s", target, full.Body.String())
				}
			}
		})
	}
}
