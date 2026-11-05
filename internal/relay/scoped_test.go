package relay

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"wanctl/internal/delegation"
	"wanctl/internal/sessionauth"
)

func testScopedGrant() delegation.Access {
	return delegation.Access{
		Namespace: "alice", CredentialID: "7", Delegated: true, GrantID: "sc_7",
		ExpiresAt: time.Now().Add(time.Hour), ExecOnly: "deploy --prod",
		Devices: []delegation.Device{{Namespace: "alice", ID: "allowed", Fingerprint: "device-fp"}},
	}
}

const scopedTestToken = "wcs_0123456789abcdef0123456789abcdef01234567"

// Docs/adr/0021: the relay's half of a scoped command token -- it is admitted
// to its one device, with exec-only capabilities, and the command is stamped
// where the controller cannot reach.
func TestScopedAdmissionStampsExecOnlyAndOneDevice(t *testing.T) {
	a := testScopedGrant()
	r := New(&transportGrantStore{grants: map[string]delegation.Access{scopedTestToken: a}})
	r.agents["alice/allowed"] = &agentConn{ns: "alice", device: "allowed", name: "allowed", delegation: true, scopedExec: true}
	r.agents["alice/hidden"] = &agentConn{ns: "alice", device: "hidden", name: "hidden", delegation: true, scopedExec: true}
	key, auth, _, ok := r.dialAccessAllowed(a, "allowed")
	if !ok || key != "alice/allowed" || auth.Capabilities != sessionauth.Exec || auth.ExecOnly != "deploy --prod" ||
		auth.GrantID != "sc_7" || auth.ControllerFingerprint != "" {
		t.Fatalf("scoped dial: %s %+v %v", key, auth, ok)
	}
	auth.Session = "s"
	if !auth.ValidFor("allowed") {
		t.Fatal("relay stamped a session its own agent contract rejects")
	}
	for _, target := range []string{"hidden", "alice/hidden", "other/allowed"} {
		if _, _, _, ok := r.dialAccessAllowed(a, target); ok {
			t.Fatalf("scoped token reached %s", target)
		}
	}
	for _, route := range []string{"/peers", "/h/peers"} {
		rec := grantRequest(r.Handler(), "GET", route, scopedTestToken)
		if rec.Code != 200 || strings.Contains(rec.Body.String(), "hidden") || !strings.Contains(rec.Body.String(), "allowed") {
			t.Fatalf("discovery leak: %d %s", rec.Code, rec.Body.String())
		}
	}
}

func TestScopedAccessInvariantsAreEnforcedWhateverTheStoreSays(t *testing.T) {
	good := testScopedGrant()
	mutate := func(f func(*delegation.Access)) delegation.Access { a := good; f(&a); return a }
	cases := map[string]struct {
		token  string
		access delegation.Access
		ok     bool
	}{
		"valid":                  {scopedTestToken, good, true},
		"not delegated":          {scopedTestToken, mutate(func(a *delegation.Access) { a.Delegated = false }), false},
		"missing scope":          {scopedTestToken, mutate(func(a *delegation.Access) { a.ExecOnly = "" }), false},
		"scope on a plain token": {"wanctl_plain", good, false},
		"two devices": {scopedTestToken, mutate(func(a *delegation.Access) {
			a.Devices = append(a.Devices, delegation.Device{Namespace: "alice", ID: "b"})
		}), false},
		"no device":                 {scopedTestToken, mutate(func(a *delegation.Access) { a.Devices = nil }), false},
		"expired":                   {scopedTestToken, mutate(func(a *delegation.Access) { a.ExpiresAt = time.Now().Add(-time.Second) }), false},
		"no expiry":                 {scopedTestToken, mutate(func(a *delegation.Access) { a.ExpiresAt = time.Time{} }), false},
		"no grant id":               {scopedTestToken, mutate(func(a *delegation.Access) { a.GrantID = "" }), false},
		"scoped prefix, plain":      {scopedTestToken, delegation.Access{Namespace: "alice", CredentialID: "x"}, false},
		"delegated needs a binding": {"wfd_x", mutate(func(a *delegation.Access) { a.ExecOnly = "" }), false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := &transportGrantStore{grants: map[string]delegation.Access{tc.token: tc.access}}
			if _, ok := ResolveAccess(s, tc.token); ok != tc.ok {
				t.Fatalf("ResolveAccess ok=%v want %v", ok, tc.ok)
			}
		})
	}
	// A store that cannot express scope must not turn the token into a bare
	// namespace credential.
	legacy := EnvTokenStore(scopedTestToken + ":alice")
	if _, ok := legacy.Resolve(scopedTestToken); ok {
		t.Fatal("legacy store resolved a scoped token")
	}
	if _, ok := ResolveAccess(legacy, scopedTestToken); ok {
		t.Fatal("legacy ResolveAccess resolved a scoped token")
	}
}

func TestScopedTokenIsRefusedOnEveryManagementAndAgentSurface(t *testing.T) {
	s := &transportGrantStore{grants: map[string]delegation.Access{scopedTestToken: testScopedGrant()}}
	h := New(s).Handler()
	for _, route := range []struct{ method, path string }{
		{"GET", "/agent"}, {"GET", "/h/poll?device=allowed"}, {"POST", "/h/deregister?device=allowed"},
		{"GET", "/session/leaked"}, {"GET", "/agent/notify-policy?device=allowed&inst=x"}, {"POST", "/agent/events?device=allowed&inst=x"},
		{"GET", "/u/friends"}, {"POST", "/u/friends/request"}, {"GET", "/u/shares"}, {"POST", "/u/shares/grant"},
		{"POST", "/u/shares/manage"}, {"POST", "/u/shares/revoke"}, {"GET", "/u/notify"}, {"POST", "/u/notify"},
		{"GET", "/u/scoped-tokens"}, {"POST", "/u/scoped-tokens"}, {"POST", "/u/scoped-tokens/revoke"},
		{"POST", "/docs/groups"}, {"POST", "/admin/tokens/issue"}, {"POST", "/admin/enroll/mint"},
	} {
		t.Run(route.method+route.path, func(t *testing.T) {
			rec := grantRequest(h, route.method, route.path, scopedTestToken)
			if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if rec.Code == http.StatusNotFound {
				t.Fatalf("route not refused: %d", rec.Code)
			}
		})
	}
}

// /resolve is the one discovery call a controller needs before dialling; for a
// scoped token it answers about its own device and about nothing else.
func TestScopedTokenResolvesOnlyItsOwnDevice(t *testing.T) {
	r := New(&transportGrantStore{grants: map[string]delegation.Access{scopedTestToken: testScopedGrant()}})
	r.agents["alice/allowed"] = &agentConn{ns: "alice", device: "allowed", name: "allowed", delegation: true, scopedExec: true}
	r.agents["alice/hidden"] = &agentConn{ns: "alice", device: "hidden", name: "hidden", delegation: true, scopedExec: true}
	if rec := grantRequest(r.Handler(), "GET", "/resolve?target=allowed", scopedTestToken); rec.Code != 200 || !strings.Contains(rec.Body.String(), "alice/allowed") {
		t.Fatalf("own device: %d %s", rec.Code, rec.Body.String())
	}
	for _, target := range []string{"hidden", "alice/hidden", "bob/allowed"} {
		if rec := grantRequest(r.Handler(), "GET", "/resolve?target="+target, scopedTestToken); rec.Code == 200 {
			t.Fatalf("resolved %s: %s", target, rec.Body.String())
		}
	}
}

// An agent that does not say it enforces the scope must never be handed the
// token: it would ignore the field and run whatever the controller asks.
func TestScopedTokenIsNotDialledToAnAgentThatCannotEnforceIt(t *testing.T) {
	a := testScopedGrant()
	s := &transportGrantStore{grants: map[string]delegation.Access{scopedTestToken: a}}
	r := New(s)
	for name, ac := range map[string]*agentConn{
		"delegation only (pre-scoped agent)": {ns: "alice", device: "allowed", name: "allowed", delegation: true},
		"no delegation":                      {ns: "alice", device: "allowed", name: "allowed"},
	} {
		t.Run(name, func(t *testing.T) {
			if agentServes(a, ac.delegation, ac.scopedExec) {
				t.Fatal("agent without scoped_exec accepted a scoped token")
			}
		})
	}
	if !agentServes(a, true, true) {
		t.Fatal("a scoped-capable agent was refused")
	}
	if !agentServes(delegation.Access{Namespace: "alice"}, false, false) {
		t.Fatal("a full token needs no capability")
	}
	if !agentServes(testTransportGrant(), true, false) {
		t.Fatal("a WebFetch grant must keep working on agents without scoped_exec")
	}
	if agentServes(testTransportGrant(), false, false) {
		t.Fatal("a WebFetch grant must still need delegation support")
	}
	_ = r
}

func TestScopedValidFor(t *testing.T) {
	ok := sessionauth.Open{Session: "s", CallerNamespace: "a", OwnerNamespace: "a", Device: "d", Capabilities: sessionauth.Exec,
		GrantID: "sc_1", CredentialID: "1", ExpiresAt: time.Now().Add(time.Minute), ExecOnly: "x"}
	if !ok.ValidFor("d") {
		t.Fatal("valid scoped open rejected")
	}
	for name, mutate := range map[string]func(*sessionauth.Open){
		"use caps":     func(o *sessionauth.Open) { o.Capabilities = sessionauth.UseCapabilities },
		"full caps":    func(o *sessionauth.Open) { o.Capabilities = sessionauth.FullCapabilities },
		"read":         func(o *sessionauth.Open) { o.Capabilities = sessionauth.Exec | sessionauth.Read },
		"no grant":     func(o *sessionauth.Open) { o.GrantID = "" },
		"no expiry":    func(o *sessionauth.Open) { o.ExpiresAt = time.Time{} },
		"expired":      func(o *sessionauth.Open) { o.ExpiresAt = time.Now().Add(-time.Second) },
		"no cred id":   func(o *sessionauth.Open) { o.CredentialID = "" },
		"wrong device": func(o *sessionauth.Open) { o.Device = "other" },
	} {
		o := ok
		mutate(&o)
		if o.ValidFor("d") {
			t.Fatalf("%s: accepted", name)
		}
	}
	// What an agent from before scoped tokens sees: the same JSON minus the
	// field it does not know, for a grant session. Exec-only capabilities are
	// not UseCapabilities, so it drops the session.
	legacy := ok
	legacy.ExecOnly = ""
	if legacy.ValidFor("d") {
		t.Fatal("an agent that ignores exec_only would serve the session")
	}
}

// The same refusal on the wire, for both carriers an agent can be on.
func TestScopedDialAnswers409ForAgentsThatCannotEnforceIt(t *testing.T) {
	for _, tc := range []struct {
		name       string
		scopedExec bool
		wantStatus func(int) bool
	}{
		{"pre-scoped agent", false, func(c int) bool { return c == http.StatusConflict }},
		{"scoped-capable agent", true, func(c int) bool { return c != http.StatusConflict }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := New(&transportGrantStore{grants: map[string]delegation.Access{scopedTestToken: testScopedGrant()}})
			r.agents["alice/allowed"] = &agentConn{ns: "alice", device: "allowed", name: "allowed", delegation: true, scopedExec: tc.scopedExec, ctrl: discardWriter{}}
			if rec := grantRequest(r.Handler(), "GET", "/dial?target=allowed", scopedTestToken); !tc.wantStatus(rec.Code) {
				t.Fatalf("ws agent: status %d %s", rec.Code, rec.Body.String())
			}
			delete(r.agents, "alice/allowed")
			r.hagents["alice/allowed"] = &httpAgent{ns: "alice", device: "allowed", name: "allowed", delegation: true, scopedExec: tc.scopedExec,
				lastSeen: time.Now(), open: make(chan sessionauth.Open, 8), changed: make(chan struct{})}
			if rec := grantRequest(r.Handler(), "POST", "/h/dial?target=allowed", scopedTestToken); !tc.wantStatus(rec.Code) {
				t.Fatalf("http agent: status %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
