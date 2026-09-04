package relay

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wanctl/internal/sessionauth"
)

type memoryAliasAdmin struct {
	noopAdmin
	devices map[string]map[string]string
}

func (m *memoryAliasAdmin) SetDeviceAlias(namespace, device, alias string) (DeviceAlias, error) {
	alias, err := normalizeDeviceAlias(alias)
	if err != nil {
		return DeviceAlias{}, err
	}
	owned := m.devices[namespace]
	if _, ok := owned[device]; !ok {
		return DeviceAlias{}, ErrDeviceNotFound
	}
	if alias != "" {
		for name, existing := range owned {
			if strings.EqualFold(name, alias) {
				return DeviceAlias{}, ErrAliasShadowsDevice
			}
			if name != device && existing != "" && strings.EqualFold(existing, alias) {
				return DeviceAlias{}, ErrAliasTaken
			}
		}
	}
	owned[device] = alias
	return DeviceAlias{Name: device, Alias: alias}, nil
}

func (m *memoryAliasAdmin) ResolveDeviceTarget(namespace, target string) (string, bool) {
	owned := m.devices[namespace]
	if _, ok := owned[target]; ok {
		return target, true
	}
	for name, alias := range owned {
		if alias != "" && strings.EqualFold(alias, target) {
			return name, true
		}
	}
	return "", false
}

func (m *memoryAliasAdmin) ListDeviceAliases(namespace string) (map[string]string, error) {
	out := map[string]string{}
	for name, alias := range m.devices[namespace] {
		if alias != "" {
			out[name] = alias
		}
	}
	return out, nil
}

func TestNormalizeDeviceAlias(t *testing.T) {
	fortyRunes := strings.Repeat("界", 40)
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "trim", input: "  work laptop  ", want: "work laptop"},
		{name: "clear", input: " \t\n ", want: ""},
		{name: "forty runes", input: fortyRunes, want: fortyRunes},
		{name: "too long", input: fortyRunes + "界", wantErr: true},
		{name: "slash", input: "home/pc", wantErr: true},
		{name: "control", input: "home\x00pc", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeDeviceAlias(tc.input)
			if tc.wantErr {
				if !errors.Is(err, ErrAliasInvalid) {
					t.Fatalf("error = %v, want alias_invalid", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("normalizeDeviceAlias(%q) = %q, %v; want %q", tc.input, got, err, tc.want)
			}
		})
	}
}

func TestPGStoreSetDeviceAliasConflictsAreCaseInsensitive(t *testing.T) {
	p := newAdminTestPGStore(t)
	if _, err := p.SetDeviceAlias("alice", "legion", "DEVBOX"); !errors.Is(err, ErrAliasShadowsDevice) {
		t.Fatalf("device-name conflict error = %v, want alias_shadows_device", err)
	}
	if _, err := p.SetDeviceAlias("alice", "legion", "HOME"); !errors.Is(err, ErrAliasTaken) {
		t.Fatalf("alias conflict error = %v, want alias_taken", err)
	}
	got, err := p.SetDeviceAlias("alice", "legion", "  work rig  ")
	if err != nil || got != (DeviceAlias{Name: "legion", Alias: "work rig"}) {
		t.Fatalf("set alias = %+v, %v", got, err)
	}
	got, err = p.SetDeviceAlias("alice", "legion", "")
	if err != nil || got != (DeviceAlias{Name: "legion"}) {
		t.Fatalf("clear alias = %+v, %v", got, err)
	}
}

func TestPGStoreResolvesRealNamesBeforeAliases(t *testing.T) {
	p := newAdminTestPGStore(t)
	if got, ok := p.ResolveDeviceTarget("alice", "desk"); !ok || got != "desk" {
		t.Fatalf("exact target = %q, %v; want desk", got, ok)
	}
	if got, ok := p.ResolveDeviceTarget("alice", "LAPTOP"); !ok || got != "legion" {
		t.Fatalf("alias target = %q, %v; want legion", got, ok)
	}
	if got, ok := p.ResolveDeviceTarget("alice", "unknown"); ok || got != "" {
		t.Fatalf("unknown target = %q, %v", got, ok)
	}
	aliases, err := p.ListDeviceAliases("alice")
	if err != nil || aliases["legion"] != "laptop" || aliases["desk"] != "office" {
		t.Fatalf("aliases = %#v, err = %v", aliases, err)
	}
}

func TestAdminDeviceAliasValidationAndConflicts(t *testing.T) {
	store := &memoryAliasAdmin{devices: map[string]map[string]string{
		"alice": {"legion": "home", "devbox": ""},
	}}
	r := New(EnvTokenStore(""))
	r.SetAdmin(store)
	r.SetAdminSecret("secret")

	request := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/devices/alias", strings.NewReader(body))
		req.Header.Set("X-Admin-Secret", "secret")
		rr := httptest.NewRecorder()
		r.Handler().ServeHTTP(rr, req)
		return rr
	}
	for _, tc := range []struct {
		body string
		code int
		err  string
	}{
		{`{"namespace":"alice","device":"legion","alias":"bad/name"}`, http.StatusBadRequest, "alias_invalid"},
		{`{"namespace":"alice","device":"devbox","alias":"HOME"}`, http.StatusConflict, "alias_taken"},
		{`{"namespace":"alice","device":"legion","alias":"DEVBOX"}`, http.StatusConflict, "alias_shadows_device"},
	} {
		rr := request(tc.body)
		if rr.Code != tc.code || strings.TrimSpace(rr.Body.String()) != tc.err {
			t.Fatalf("POST %s = %d %q; want %d %q", tc.body, rr.Code, rr.Body.String(), tc.code, tc.err)
		}
	}

	rr := request(`{"namespace":"alice","device":"legion","alias":"  desk  "}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("valid alias status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got DeviceAlias
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil || got.Alias != "desk" {
		t.Fatalf("valid alias response = %+v, decode = %v", got, err)
	}
}

type recordingACL struct {
	caller, owner, device string
}

func (a *recordingACL) ACLPerms(caller, owner, device string) (string, bool) {
	a.caller, a.owner, a.device = caller, owner, device
	return "read", true
}

func TestDialAllowedResolvesDeviceAliases(t *testing.T) {
	store := &memoryAliasAdmin{devices: map[string]map[string]string{
		"alice": {"legion": "desk", "desk": "other"},
		"owner": {"home-pc": "Laptop"},
	}}
	r := New(EnvTokenStore(""))
	r.SetAdmin(store)

	key, auth, ok := r.dialAllowed("alice", "desk")
	if !ok || key != "alice/desk" || auth.Device != "desk" {
		t.Fatalf("real-name priority = key %q auth %+v ok %v", key, auth, ok)
	}

	acl := &recordingACL{}
	r.SetACL(acl)
	key, auth, ok = r.dialAllowed("reader", "owner/laptop")
	if !ok || key != "owner/home-pc" || auth.Device != "home-pc" || auth.OwnerNamespace != "owner" {
		t.Fatalf("cross-namespace alias = key %q auth %+v ok %v", key, auth, ok)
	}
	if acl.caller != "reader" || acl.owner != "owner" || acl.device != "home-pc" || auth.Capabilities != sessionauth.Read {
		t.Fatalf("ACL saw unresolved target: acl=%+v auth=%+v", acl, auth)
	}

	withoutAdmin := New(EnvTokenStore(""))
	withoutAdminACL := &recordingACL{}
	withoutAdmin.SetACL(withoutAdminACL)
	key, auth, ok = withoutAdmin.dialAllowed("reader", "owner/laptop")
	if !ok || key != "owner/laptop" || auth.Device != "laptop" || withoutAdminACL.device != "laptop" {
		t.Fatalf("static-token behavior changed: key %q auth %+v ok %v", key, auth, ok)
	}
}

func TestPeerEndpointsIncludeOnlineAliases(t *testing.T) {
	store := &memoryAliasAdmin{devices: map[string]map[string]string{
		"alice": {"ws-box": "workstation", "plain-box": "", "offline-box": "offline"},
	}}
	r := New(EnvTokenStore("tok:alice"))
	r.SetAdmin(store)
	r.agents["alice/ws-box"] = &agentConn{ns: "alice", device: "ws-box"}
	r.agents["alice/plain-box"] = &agentConn{ns: "alice", device: "plain-box"}

	for _, path := range []string{"/peers", "/h/peers"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer tok")
		rr := httptest.NewRecorder()
		r.Handler().ServeHTTP(rr, req)
		var got struct {
			Devices []string          `json:"devices"`
			Aliases map[string]string `json:"aliases"`
		}
		if err := json.NewDecoder(rr.Body).Decode(&got); err != nil || rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, decode = %v", path, rr.Code, err)
		}
		if got.Aliases["ws-box"] != "workstation" || len(got.Aliases) != 1 {
			t.Fatalf("GET %s aliases = %#v", path, got.Aliases)
		}
	}
}
