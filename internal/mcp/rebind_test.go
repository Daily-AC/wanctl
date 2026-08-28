package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wanctl/internal/admission"
	"wanctl/internal/transport"

	mcpapi "github.com/mark3labs/mcp-go/mcp"
)

func newRemoteRebindTestSession(seed []byte) (*sessionStore, *remoteSession) {
	store := &sessionStore{
		seed:    append([]byte(nil), seed...),
		m:       map[string]*remoteSession{},
		revoked: map[string]time.Time{},
	}
	r := &remoteSession{
		id: "default", seed: store.seed, owner: store, known: transport.NewMemStore(),
		rebindJTIs: map[string]time.Time{}, lastUsed: time.Now(),
	}
	store.m["default"] = r
	sessions = store
	return store, r
}

func rebindLoginRequest(credential string) mcpapi.CallToolRequest {
	return mcpapi.CallToolRequest{Params: mcpapi.CallToolParams{
		Name:      "wanctl_login",
		Arguments: map[string]any{"rebind": credential},
	}}
}

func TestRebindCredentialDoesNotExposeRawToken(t *testing.T) {
	const token = "wanctl_super_secret_raw_token"
	credential, _, err := sealRebind([]byte(strings.Repeat("s", 32)), "alice", token, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(credential, rebindPrefix))
	if err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if strings.Contains(string(payload), token) {
		t.Fatal("rebind credential exposes the raw relay token after base64 decoding")
	}
}

func TestRebindSealedClaimRoundTrip(t *testing.T) {
	seed := []byte(strings.Repeat("s", 32))
	now := time.Unix(1_800_000_000, 0)
	credential, issued, err := sealRebind(seed, "alice", "token", now)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := openRebind(seed, credential, now)
	if err != nil {
		t.Fatal(err)
	}
	if claim != issued {
		t.Fatalf("opened claim = %+v, issued = %+v", claim, issued)
	}
	if claim.Version != rebindVersion || claim.Audience != rebindAudience || claim.JTI == "" {
		t.Fatalf("required claims missing: %+v", claim)
	}
	if claim.ExpiresAt != now.Add(rebindTTL).Unix() {
		t.Fatalf("expiry = %d", claim.ExpiresAt)
	}
}

func TestRebindTamperAndExpiryFail(t *testing.T) {
	seed := []byte(strings.Repeat("s", 32))
	now := time.Now()
	credential, _, err := sealRebind(seed, "alice", "token", now)
	if err != nil {
		t.Fatal(err)
	}
	tampered := []byte(credential)
	i := len(rebindPrefix) + (len(tampered)-len(rebindPrefix))/2
	if tampered[i] == 'A' {
		tampered[i] = 'B'
	} else {
		tampered[i] = 'A'
	}
	if _, err := openRebind(seed, string(tampered), now); !errors.Is(err, ErrInvalidRebind) {
		t.Fatalf("tampered credential error = %v", err)
	}
	if _, err := openRebind(seed, credential, now.Add(rebindTTL)); !errors.Is(err, ErrExpiredRebind) {
		t.Fatalf("expired credential error = %v", err)
	}
}

func TestLegacyBase64RebindIsRejected(t *testing.T) {
	legacy := base64.RawURLEncoding.EncodeToString([]byte("alice\x00raw-token"))
	if _, err := openRebind([]byte(strings.Repeat("s", 32)), legacy, time.Now()); !errors.Is(err, ErrLegacyRebind) {
		t.Fatalf("legacy credential error = %v", err)
	}
	newRemoteRebindTestSession([]byte(strings.Repeat("s", 32)))
	result, err := mcpLogin(context.Background(), rebindLoginRequest(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("legacy credential should require a fresh login")
	}
	text, ok := result.Content[0].(mcpapi.TextContent)
	if !ok || !strings.Contains(text.Text, "run wanctl_login() again") {
		t.Fatalf("legacy rejection did not instruct a fresh login: %+v", result.Content)
	}
}

func TestRebindDoesNotTrustClaimedNamespace(t *testing.T) {
	var resolvedToken string
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resolvedToken, _, _ = admission.Token(r)
		json.NewEncoder(w).Encode(map[string]any{"namespace": "real-owner", "devices": []string{}})
	}))
	defer relay.Close()
	t.Setenv("WANCTL_RELAY", relay.URL)
	t.Setenv("WANCTL_TRANSPORT", "ws")

	seed := []byte(strings.Repeat("s", 32))
	_, r := newRemoteRebindTestSession(seed)
	credential, _, err := sealRebind(seed, "forged-owner", "valid-token", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	result, err := mcpLogin(context.Background(), rebindLoginRequest(credential))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("rebind trusted the sealed claim's namespace without matching the Relay")
	}
	if resolvedToken != "valid-token" {
		t.Fatalf("Relay resolved token %q", resolvedToken)
	}
	if r.namespace != "" || r.identity != nil {
		t.Fatalf("failed rebind saved namespace %q or identity", r.namespace)
	}
}

func TestRebindLogoutRevocationIsProcessLocal(t *testing.T) {
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"namespace": "alice", "devices": []string{}})
	}))
	defer relay.Close()
	t.Setenv("WANCTL_RELAY", relay.URL)
	t.Setenv("WANCTL_TRANSPORT", "ws")

	seed := []byte(strings.Repeat("s", 32))
	_, r := newRemoteRebindTestSession(seed)
	credential, err := r.saveLoginAndIssue("valid-token", "alice", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mcpLogout(context.Background(), mcpapi.CallToolRequest{}); err != nil {
		t.Fatal(err)
	}
	result, err := mcpLogin(context.Background(), rebindLoginRequest(credential))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("logout did not revoke the issued JTI in the current process")
	}

	// Revocations are intentionally process-local: a fresh server store has no
	// durable deny-list. The still-valid Relay token and unexpired sealed claim
	// can be restored after a process restart.
	newRemoteRebindTestSession(seed)
	result, err = mcpLogin(context.Background(), rebindLoginRequest(credential))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatal("fresh process boundary unexpectedly retained the in-memory JTI revocation")
	}
}
