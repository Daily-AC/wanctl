package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wanctl/internal/agent"
	"wanctl/internal/policy"
	"wanctl/internal/relay"
	"wanctl/internal/transport"
)

func trustServer(t *testing.T, c *Client, target string) {
	t.Helper()
	_, _, err := c.Pair(context.Background(), target)
	var required *TrustRequiredError
	if !errors.As(err, &required) {
		t.Fatalf("first contact: want TrustRequiredError, got %v", err)
	}
	if required.Target == "" || required.Fingerprint == "" {
		t.Fatalf("incomplete trust request: %+v", required)
	}
	if target == "home-pc" && (!strings.HasPrefix(required.Target, "alice/") || !transport.ValidDeviceID(strings.TrimPrefix(required.Target, "alice/"))) {
		t.Fatalf("trust request target = %q, want canonical alice/<device ID>", required.Target)
	}
	if _, err := c.PinServer(context.Background(), required.Target, required.Fingerprint, false); err != nil {
		t.Fatalf("confirm server identity: %v", err)
	}
}

func TestClientExecAndFileRoundTrip(t *testing.T) {
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	// Agent.
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	ag, err := agent.New(agent.Options{RelayURL: base, Token: "tok", Name: "home-pc", AutoYes: true, Mode: policy.ModeBypass, Version: "v1.2.3-test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)
	time.Sleep(200 * time.Millisecond)

	// Controller (separate config dir).
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", base)
	t.Setenv("WANCTL_TOKEN", "tok")
	t.Setenv("WANCTL_TRANSPORT", "ws") // this test wires a ws relay; New() now defaults to http
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	trustServer(t, c, "home-pc")
	status, err := c.Status(context.Background(), "home-pc")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Name != "home-pc" || status.Mode != policy.ModeBypass || status.Version != "v1.2.3-test" || !status.Detailed {
		t.Fatalf("status = %+v", status)
	}

	code, err := c.Exec(context.Background(), ExecRequest{Target: "home-pc", Command: "echo hi", OneShot: true, Cwd: ""})
	if err != nil || code != 0 {
		t.Fatalf("exec: code=%d err=%v", code, err)
	}

	// Push then pull a file, verify contents survive the round trip.
	local := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(local, []byte("payload-123"), 0o644)
	remote := filepath.Join(t.TempDir(), "remote.txt")
	if err := c.Push(context.Background(), "home-pc", local, remote); err != nil {
		t.Fatalf("push: %v", err)
	}
	back := filepath.Join(t.TempDir(), "back.txt")
	if err := c.Pull(context.Background(), "home-pc", remote, back); err != nil {
		t.Fatalf("pull: %v", err)
	}
	got, _ := os.ReadFile(back)
	if string(got) != "payload-123" {
		t.Fatalf("round trip mismatch: %q", got)
	}
}

func TestNewWithWiring(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	id, err := transport.LoadOrCreateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	known, _ := transport.OpenStore("known_servers.json")
	c := NewWith(id, known, "wss://relay.example/", "tok", "http")
	if c.relayURL != "wss://relay.example" || c.token != "tok" || c.transport != "http" {
		t.Fatalf("wiring: %+v", c)
	}
}

func TestPinNamePreservesOwnerNamespace(t *testing.T) {
	alice := pinName("alice/build")
	bob := pinName("bob/build")
	if alice != "alice/build" || bob != "bob/build" || alice == bob {
		t.Fatalf("pin keys collide: alice=%q bob=%q", alice, bob)
	}
}

func TestResolveUnqualifiedTargetUsesRelayNamespace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/resolve" {
			http.NotFound(w, r)
			return
		} // legacy relay
		json.NewEncoder(w).Encode(map[string]any{"namespace": "alice", "devices": []string{"build"}})
	}))
	defer srv.Close()
	c := NewWith(nil, transport.NewMemStore(), srv.URL, "tok", "http")
	fp := transport.Fingerprint([]byte("device cert"))
	target, err := c.PinServer(context.Background(), "build", fp, false)
	if err != nil {
		t.Fatal(err)
	}
	if target != "alice/build" {
		t.Fatalf("canonical target = %q, want alice/build", target)
	}
	if got, ok := c.known.GetByName("alice/build"); !ok || got.Fingerprint != fp {
		t.Fatalf("canonical pin missing: %+v, ok=%v", got, ok)
	}
}

func TestPeersWithAliasesAndAcceptedTargetSpellings(t *testing.T) {
	c := NewWith(nil, transport.NewMemStore(), "https://relay.test", "tok", "http")
	c.httpc = &http.Client{Transport: peerRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"namespace":"alice","devices":["legion","plain"],"aliases":{"legion":"desk"}}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})}

	devices, aliases, err := c.PeersWithAliases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(devices, ",") != "legion,plain" || aliases["legion"] != "desk" {
		t.Fatalf("peers = %v, aliases = %#v", devices, aliases)
	}
	spellings, err := c.PeerAliases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := "legion,alice/legion,desk,alice/desk,plain,alice/plain"
	if got := strings.Join(spellings, ","); got != want {
		t.Fatalf("target spellings = %q, want %q", got, want)
	}
}

type peerRoundTripFunc func(*http.Request) (*http.Response, error)

func (f peerRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Dialing by alias must land on the pin the hostname already has. The store is
// keyed by the target string as typed, so without alias→name resolution the
// first `--target 客厅` re-asks for a fingerprint the operator already accepted
// for that exact machine — a confirmation prompt that can only ever say yes.
func TestResolveMapsAliasOntoTheDeviceItsHostnamePinned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/resolve" {
			http.NotFound(w, r)
			return
		} // legacy relay
		json.NewEncoder(w).Encode(map[string]any{
			"namespace": "alice",
			"devices":   []string{"bench-02", "atlas"},
			"aliases":   map[string]string{"bench-02": "客厅"},
		})
	}))
	defer srv.Close()
	c := NewWith(nil, transport.NewMemStore(), srv.URL, "tok", "http")
	fp := transport.Fingerprint([]byte("device cert"))

	if _, err := c.PinServer(context.Background(), "bench-02", fp, false); err != nil {
		t.Fatal(err)
	}
	target, err := c.resolve(context.Background(), "客厅")
	if err != nil {
		t.Fatal(err)
	}
	if target != "alice/bench-02" {
		t.Fatalf("alias resolved to %q, want alice/bench-02", target)
	}
	if got, ok := c.known.GetByName(target); !ok || got.Fingerprint != fp {
		t.Fatalf("alias dial missed the hostname pin: %+v, ok=%v", got, ok)
	}

	// A real device name outranks an alias, matching how the relay resolves it.
	if got, err := c.resolve(context.Background(), "atlas"); err != nil || got != "alice/atlas" {
		t.Fatalf("resolve(atlas) = %q, %v", got, err)
	}
}

// A grantee's only way to learn the owner namespace is this list, so it has to
// arrive intact alongside their own devices - and an older relay that omits it
// must not break the call.
func TestPeersAndSharedCarriesGrantedDevices(t *testing.T) {
	respond := func(body string) *Client {
		c := NewWith(nil, transport.NewMemStore(), "https://relay.test", "tok", "http")
		c.httpc = &http.Client{Transport: peerRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		})}
		return c
	}
	c := respond(`{"namespace":"waerjili123","devices":["mine"],"aliases":{},
	  "shared":[{"owner":"daily-ac","device":"8e894048","label":"bms-20558674","target":"daily-ac/8e894048","online":true}]}`)
	devices, _, shared, err := c.PeersAndShared(context.Background())
	if err != nil || len(devices) != 1 || len(shared) != 1 {
		t.Fatalf("peers = %v, shared = %+v, err = %v", devices, shared, err)
	}
	if shared[0].Target != "daily-ac/8e894048" || shared[0].Label != "bms-20558674" || !shared[0].Online {
		t.Fatalf("shared device = %+v", shared[0])
	}
	spellings, err := c.PeerAliases(context.Background())
	if err != nil || !slicesContain(spellings, "daily-ac/8e894048") || !slicesContain(spellings, "daily-ac/bms-20558674") {
		t.Fatalf("target completion = %v, err = %v", spellings, err)
	}

	old := respond(`{"namespace":"alice","devices":["legion"],"aliases":{}}`)
	if _, _, shared, err := old.PeersAndShared(context.Background()); err != nil || len(shared) != 0 {
		t.Fatalf("relay without shared support = %+v, %v", shared, err)
	}
}

func slicesContain(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// The relay explains a refused target ("no device ... run wanctl peers"); the
// controller used to print only the status code, which told the user nothing.
func TestRelayExplanationKeepsPlainTextAndDropsEverythingElse(t *testing.T) {
	body := func(s string) *http.Response {
		return &http.Response{Body: io.NopCloser(strings.NewReader(s))}
	}
	msg := `no device "bms-20558674" in namespace "waerjili123"; a device shared with you is addressed as owner/device`
	if got := relayExplanation(body(msg + "\n")); got != msg {
		t.Fatalf("explanation = %q", got)
	}
	for _, in := range []string{"", "   ", "<html><body>502 Bad Gateway</body></html>", strings.Repeat("x", 500), "bad\x00text"} {
		if got := relayExplanation(body(in)); got != "" {
			t.Fatalf("relayExplanation(%.20q) = %q, want empty", in, got)
		}
	}
	if got := relayExplanation(nil); got != "" {
		t.Fatalf("nil response = %q", got)
	}
}
