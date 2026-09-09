package relay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sharedTestDevice is one row of the devices table plus, optionally, one grant
// on it. The relay only ever sees these through AdminStore.
type sharedTestDevice struct {
	owner, id, label, alias string
	grantee, perms          string
	manage                  bool
}

type memSharedAdmin struct {
	noopAdmin
	devices []sharedTestDevice
}

func (m *memSharedAdmin) ListDevices(ns string) ([]map[string]any, error) {
	out := []map[string]any{}
	for _, d := range m.devices {
		shared := d.owner != ns && d.grantee == ns
		if d.owner != ns && !shared {
			continue
		}
		row := map[string]any{
			"name": d.id, "alias": d.alias, "owner": d.owner, "shared": shared,
			"display_name": d.label, "legacy_name": "", "device_id": d.id,
		}
		if shared {
			row["perms"] = d.perms
		}
		out = append(out, row)
	}
	return out, nil
}

func (m *memSharedAdmin) ResolveDeviceTargetStrict(ns, target string) (string, error) {
	var matches []string
	for _, d := range m.devices {
		if d.owner != ns {
			continue
		}
		if d.id == target || strings.EqualFold(d.label, target) || (d.alias != "" && strings.EqualFold(d.alias, target)) {
			matches = append(matches, d.id)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", ErrDeviceNotFound
	default:
		return "", fmt.Errorf("ambiguous device name; use a device ID")
	}
}

func (m *memSharedAdmin) ResolveDeviceTarget(ns, target string) (string, bool) {
	id, err := m.ResolveDeviceTargetStrict(ns, target)
	return id, err == nil
}

func (m *memSharedAdmin) ListDeviceAliases(ns string) (map[string]string, error) {
	out := map[string]string{}
	for _, d := range m.devices {
		if d.owner == ns && d.alias != "" {
			out[d.id] = d.alias
		}
	}
	return out, nil
}

func (m *memSharedAdmin) SetDeviceAlias(string, string, string) (DeviceAlias, error) {
	return DeviceAlias{}, ErrDeviceNotFound
}

func (m *memSharedAdmin) ACLGrant(caller, owner, device string) (Grant, bool) {
	for _, d := range m.devices {
		if d.owner == owner && d.id == device && d.grantee == caller && d.perms != "" {
			return Grant{Manage: d.manage}, true
		}
	}
	return Grant{}, false
}

// The reported case: daily-ac shared bms-20558674 with waerjili123, whose token
// could see nothing but its own namespace and therefore could not learn the
// owner namespace that --target needs.
func sharedFixture() (*Relay, *memSharedAdmin) {
	store := &memSharedAdmin{devices: []sharedTestDevice{
		{owner: "waerjili123", id: "11111111-1111-4111-8111-111111111111", label: "my-laptop"},
		{owner: "daily-ac", id: "8e894048-2222-4333-8444-555566667777", label: "bms-20558674",
			grantee: "waerjili123", perms: "exec,read,write"},
		{owner: "daily-ac", id: "99999999-9999-4999-8999-999999999999", label: "not-yours"},
	}}
	r := New(EnvTokenStore("grantee-token:waerjili123"))
	r.SetAdmin(store)
	r.SetACL(store)
	r.agents["waerjili123/11111111-1111-4111-8111-111111111111"] = &agentConn{ns: "waerjili123", device: "11111111-1111-4111-8111-111111111111"}
	r.agents["daily-ac/8e894048-2222-4333-8444-555566667777"] = &agentConn{ns: "daily-ac", device: "8e894048-2222-4333-8444-555566667777"}
	return r, store
}

func TestPeerEndpointsListDevicesSharedWithTheCaller(t *testing.T) {
	r, _ := sharedFixture()
	for _, path := range []string{"/peers", "/h/peers"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer grantee-token")
		rr := httptest.NewRecorder()
		r.Handler().ServeHTTP(rr, req)
		var got struct {
			Namespace string            `json:"namespace"`
			Devices   []string          `json:"devices"`
			Aliases   map[string]string `json:"aliases"`
			Shared    []SharedPeer      `json:"shared"`
		}
		if err := json.NewDecoder(rr.Body).Decode(&got); err != nil || rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, decode = %v", path, rr.Code, err)
		}
		// The caller's own namespace is reported exactly as before.
		if len(got.Devices) != 1 || got.Devices[0] != "11111111-1111-4111-8111-111111111111" {
			t.Fatalf("GET %s own devices = %#v", path, got.Devices)
		}
		if len(got.Shared) != 1 {
			t.Fatalf("GET %s shared = %#v", path, got.Shared)
		}
		want := SharedPeer{
			Owner: "daily-ac", Device: "8e894048-2222-4333-8444-555566667777",
			Label: "bms-20558674", Target: "daily-ac/8e894048-2222-4333-8444-555566667777",
			Online: true,
		}
		if got.Shared[0] != want {
			t.Fatalf("GET %s shared[0] = %+v, want %+v", path, got.Shared[0], want)
		}
	}
}

// A device nobody shared with the caller must not appear, and a relay with no
// admin database must keep answering exactly as it did.
func TestPeerEndpointsOmitUnsharedAndStoreLessNamespaces(t *testing.T) {
	r, _ := sharedFixture()
	for _, peer := range r.sharedPeers("waerjili123") {
		if peer.Device == "99999999-9999-4999-8999-999999999999" {
			t.Fatal("a device with no grant leaked into the shared list")
		}
	}
	if peers := r.sharedPeers("daily-ac"); len(peers) != 0 {
		t.Fatalf("an owner is not a grantee of its own devices: %+v", peers)
	}
	static := New(EnvTokenStore("tok:alice"))
	req := httptest.NewRequest(http.MethodGet, "/peers", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rr := httptest.NewRecorder()
	static.Handler().ServeHTTP(rr, req)
	if body := strings.TrimSpace(rr.Body.String()); strings.Contains(body, "shared") {
		t.Fatalf("static-token relay grew a shared field: %s", body)
	}
}

func TestDialAllowedResolvesBareNamesAgainstGrants(t *testing.T) {
	r, _ := sharedFixture()
	const sharedID = "8e894048-2222-4333-8444-555566667777"

	// The exact spelling the grantee's agent tried, plus the two that work.
	for _, target := range []string{"bms-20558674", "BMS-20558674", sharedID, "daily-ac/" + sharedID} {
		key, auth, reason, ok := r.dialAllowedReason("waerjili123", target)
		if !ok || key != "daily-ac/"+sharedID || auth.OwnerNamespace != "daily-ac" {
			t.Fatalf("target %q = key %q auth %+v reason %q ok %v", target, key, auth, reason, ok)
		}
	}
	// A bare label that matches one of the caller's own devices still means
	// their own device: a grant must never shadow what they own.
	if key, _, _, ok := r.dialAllowedReason("waerjili123", "my-laptop"); !ok || key != "waerjili123/11111111-1111-4111-8111-111111111111" {
		t.Fatalf("own device = %q, ok %v", key, ok)
	}
}

func TestDialRefusalExplainsOwnNamespaceAndStaysSilentElsewhere(t *testing.T) {
	r, _ := sharedFixture()

	// The mistake in the report: the shared device addressed as if it were the
	// grantee's own. Naming their own namespace discloses nothing.
	for _, target := range []string{"bms-not-a-device", "waerjili123/bms-20558674"} {
		_, _, reason, ok := r.dialAllowedReason("waerjili123", target)
		if ok {
			t.Fatalf("target %q was allowed", target)
		}
		if !strings.Contains(reason, "owner/device") || !strings.Contains(reason, "wanctl peers") || !strings.Contains(reason, "waerjili123") {
			t.Fatalf("target %q reason = %q", target, reason)
		}
	}

	// Someone else's namespace must not answer whether a device is there.
	for _, target := range []string{"daily-ac/99999999-9999-4999-8999-999999999999", "daily-ac/no-such-device"} {
		if _, _, reason, ok := r.dialAllowedReason("waerjili123", target); ok || reason != "" {
			t.Fatalf("target %q leaked %q (allowed=%v)", target, reason, ok)
		}
		if got := dialRefusal(""); got != "forbidden" {
			t.Fatalf("bare refusal = %q", got)
		}
	}
}

// Two owners may label their devices the same. Guessing between them would hand
// a session to the wrong machine, so a bare label that matches both is refused.
func TestDialAllowedRefusesLabelsSharedByTwoNamespaces(t *testing.T) {
	store := &memSharedAdmin{devices: []sharedTestDevice{
		{owner: "daily-ac", id: "aaaa1111-1111-4111-8111-111111111111", label: "buildbox", grantee: "bob", perms: "read"},
		{owner: "acme", id: "bbbb2222-2222-4222-8222-222222222222", label: "buildbox", grantee: "bob", perms: "read"},
	}}
	r := New(EnvTokenStore("t:bob"))
	r.SetAdmin(store)
	r.SetACL(store)

	_, _, reason, ok := r.dialAllowedReason("bob", "buildbox")
	if ok {
		t.Fatal("an ambiguous shared label was resolved")
	}
	if !strings.Contains(reason, "more than one namespace") ||
		!strings.Contains(reason, "daily-ac/aaaa1111-1111-4111-8111-111111111111") ||
		!strings.Contains(reason, "acme/bbbb2222-2222-4222-8222-222222222222") {
		t.Fatalf("ambiguity reason = %q", reason)
	}
}

// The relay's explanation has to survive the trip to the user: /resolve runs
// before any dial, so it is where a mistyped target is first rejected.
func TestResolveAndDialEndpointsReturnTheRefusalText(t *testing.T) {
	r, _ := sharedFixture()
	for _, path := range []string{"/resolve?target=bms-20558674", "/dial?target=bms-not-a-device", "/h/dial?target=bms-not-a-device"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer grantee-token")
		rr := httptest.NewRecorder()
		r.Handler().ServeHTTP(rr, req)
		body := strings.TrimSpace(rr.Body.String())
		if strings.HasPrefix(path, "/resolve") {
			// This one resolves through the grant and succeeds.
			if rr.Code != http.StatusOK || !strings.Contains(body, "daily-ac/8e894048-2222-4333-8444-555566667777") {
				t.Fatalf("GET %s = %d %q", path, rr.Code, body)
			}
			continue
		}
		if rr.Code != http.StatusForbidden || !strings.Contains(body, "wanctl peers") {
			t.Fatalf("GET %s = %d %q", path, rr.Code, body)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/resolve?target=bms-not-a-device", nil)
	req.Header.Set("Authorization", "Bearer grantee-token")
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "owner/device") {
		t.Fatalf("GET /resolve unknown = %d %q", rr.Code, rr.Body.String())
	}
}

// The in-memory store above is a stand-in. This one runs the real query that
// decides what a grantee can see, against a real PostgreSQL server.
func TestPGSharedPeersAndBareTargetResolutionAcrossGrants(t *testing.T) {
	db := deviceIDTestDB(t)
	if err := runMigrations(db, migrationFiles); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	const sharedID = "8e894048-2222-4333-8444-555566667777"
	exec(`INSERT INTO devices(owner_namespace,device_id,display_name,uses_device_id) VALUES ('daily-ac',$1,'bms-20558674',true)`, sharedID)
	exec(`INSERT INTO devices(owner_namespace,device_id,display_name,uses_device_id) VALUES ('daily-ac','99999999-9999-4999-8999-999999999999','not-yours',true)`)
	exec(`INSERT INTO devices(owner_namespace,device_id,display_name,uses_device_id) VALUES ('waerjili123','11111111-1111-4111-8111-111111111111','my-laptop',true)`)
	exec(`INSERT INTO acl(owner_namespace,device,grantee_namespace,perms) VALUES ('daily-ac',$1,'waerjili123','exec,read,write')`, sharedID)

	p := &PGStore{db: db}
	r := New(EnvTokenStore("grantee-token:waerjili123"))
	r.SetAdmin(p)
	r.SetACL(p)
	r.agents["daily-ac/"+sharedID] = &agentConn{ns: "daily-ac", device: sharedID}

	peers := r.sharedPeers("waerjili123")
	if len(peers) != 1 || peers[0].Target != "daily-ac/"+sharedID || peers[0].Label != "bms-20558674" || !peers[0].Online {
		t.Fatalf("shared peers = %+v", peers)
	}
	key, auth, reason, ok := r.dialAllowedReason("waerjili123", "bms-20558674")
	if !ok || key != "daily-ac/"+sharedID || auth.OwnerNamespace != "daily-ac" {
		t.Fatalf("bare shared target = key %q auth %+v reason %q ok %v", key, auth, reason, ok)
	}
	if _, _, reason, ok := r.dialAllowedReason("waerjili123", "not-yours"); ok || !strings.Contains(reason, "wanctl peers") {
		t.Fatalf("ungranted device = reason %q ok %v", reason, ok)
	}
	exec(`UPDATE acl SET revoked_at = now() WHERE grantee_namespace='waerjili123'`)
	if peers := r.sharedPeers("waerjili123"); len(peers) != 0 {
		t.Fatalf("a revoked grant still listed the device: %+v", peers)
	}
	if _, _, _, ok := r.dialAllowedReason("waerjili123", "bms-20558674"); ok {
		t.Fatal("a revoked grant still resolved a bare target")
	}
}

// Migration 008 and the switch it adds, against a real PostgreSQL server: a
// share starts unmanaged, the owner can turn management on without revoking,
// and a revoked row stops counting either way.
func TestPGACLManageSwitchRoundTrip(t *testing.T) {
	db := deviceIDTestDB(t)
	if err := runMigrations(db, migrationFiles); err != nil {
		t.Fatal(err)
	}
	var isNullable, dflt string
	err := db.QueryRow(`SELECT is_nullable, column_default FROM information_schema.columns
	                     WHERE table_name='acl' AND column_name='manage'`).Scan(&isNullable, &dflt)
	if err != nil || isNullable != "NO" || dflt != "false" {
		t.Fatalf("acl.manage = nullable %q default %q, err %v", isNullable, dflt, err)
	}

	p := &PGStore{db: db}
	const device = "8e894048-2222-4333-8444-555566667777"
	if _, err := db.Exec(`INSERT INTO devices(owner_namespace,device_id,display_name,uses_device_id) VALUES ('daily-ac',$1,'bms',true)`, device); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO friends(requester_ns,addressee_ns,status,accepted_at) VALUES ('daily-ac','waerjili123','accepted',now())`); err != nil {
		t.Fatal(err)
	}

	// A share is unmanaged unless it is asked for.
	if _, err := p.GrantACL("daily-ac", device, "waerjili123", false); err != nil {
		t.Fatal(err)
	}
	grant, ok := p.ACLGrant("waerjili123", "daily-ac", device)
	if !ok || grant.Manage {
		t.Fatalf("a new share = %+v, %v; want an unmanaged grant", grant, ok)
	}

	// The owner changes their mind without revoking, so the grantee does not
	// have to pair with the device again.
	changed, err := p.SetACLManage("daily-ac", device, "waerjili123", true)
	if err != nil || !changed {
		t.Fatalf("turning management on: changed=%v err=%v", changed, err)
	}
	if grant, ok := p.ACLGrant("waerjili123", "daily-ac", device); !ok || !grant.Manage {
		t.Fatalf("after turning it on = %+v, %v", grant, ok)
	}
	if changed, err := p.SetACLManage("daily-ac", device, "waerjili123", false); err != nil || !changed {
		t.Fatalf("turning management off: changed=%v err=%v", changed, err)
	}
	if grant, ok := p.ACLGrant("waerjili123", "daily-ac", device); !ok || grant.Manage {
		t.Fatalf("after turning it off = %+v, %v", grant, ok)
	}

	// Someone else's namespace is not a share, and neither is a revoked one.
	if changed, err := p.SetACLManage("daily-ac", device, "stranger", true); err != nil || changed {
		t.Fatalf("flipped a share that does not exist: changed=%v err=%v", changed, err)
	}
	if _, err := db.Exec(`UPDATE acl SET revoked_at = now()`); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.ACLGrant("waerjili123", "daily-ac", device); ok {
		t.Fatal("a revoked share still granted access")
	}
	if changed, err := p.SetACLManage("daily-ac", device, "waerjili123", true); err != nil || changed {
		t.Fatalf("a revoked share was re-enabled by flipping the switch: changed=%v err=%v", changed, err)
	}
}

// The listings the portal and the CLI read must carry the switch, or an owner
// cannot see which of their shares can administer the device.
func TestPGShareListingsCarryTheManagementSwitch(t *testing.T) {
	db := deviceIDTestDB(t)
	if err := runMigrations(db, migrationFiles); err != nil {
		t.Fatal(err)
	}
	p := &PGStore{db: db}
	if _, err := db.Exec(`INSERT INTO devices(owner_namespace,device_id,display_name,uses_device_id) VALUES ('alice','dev-a','a',true),('alice','dev-b','b',true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO friends(requester_ns,addressee_ns,status,accepted_at) VALUES ('alice','bob','accepted',now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.GrantACL("alice", "dev-a", "bob", true); err != nil {
		t.Fatal(err)
	}
	if _, err := p.GrantACL("alice", "dev-b", "bob", false); err != nil {
		t.Fatal(err)
	}

	given, err := p.ListACL("alice")
	if err != nil || len(given) != 2 {
		t.Fatalf("owner listing = %+v %v", given, err)
	}
	for _, row := range given {
		want := row["device"] == "dev-a"
		if row["manage"] != want {
			t.Fatalf("owner listing row %+v: manage should be %v", row, want)
		}
	}

	received, err := p.ListReceivedACL("bob")
	if err != nil || len(received) != 2 {
		t.Fatalf("grantee listing = %+v %v", received, err)
	}
	for _, share := range received {
		if share.Manage != (share.Device == "dev-a") {
			t.Fatalf("grantee listing %+v carries the wrong switch", share)
		}
	}

	devices, err := p.ListDevices("bob")
	if err != nil || len(devices) != 2 {
		t.Fatalf("grantee device list = %+v %v", devices, err)
	}
	for _, row := range devices {
		if row["manage"] != (row["name"] == "dev-a") {
			t.Fatalf("device row %+v carries the wrong switch", row)
		}
	}
}
