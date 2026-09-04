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
	if required.Target != "alice/home-pc" && target == "home-pc" {
		t.Fatalf("trust request target = %q, want canonical alice/home-pc", required.Target)
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
