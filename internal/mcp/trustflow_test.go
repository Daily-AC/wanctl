package mcp

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"wanctl/internal/client"
	"wanctl/internal/relay"
	"wanctl/internal/transport"

	mcpapi "github.com/mark3labs/mcp-go/mcp"
)

const (
	trustTarget = "alice/build"
	pinnedFP    = "SHA256:PkBvOpK0S0fRZ0eS2Ib3EGQMhVPfsoTrN0Fxo3gIEvE="
	offeredFP   = "SHA256:8Jq1qkbc6HW9FzS2eiXW9ZQe1bFqDMjJmJ0mkLJ6t8Y="
)

// The whole point of the change: over HTTP the reader is a model, and the CLI
// sentence it used to get names a command that does not exist on this surface.
// It must instead be told the tool to call, with both values it has to pass.
func TestTrustRequiredOverHTTPNamesTheToolAndBothValues(t *testing.T) {
	err := &client.TrustRequiredError{Target: trustTarget, Fingerprint: pinnedFP}
	res := dialErrorResult(&remoteSession{}, err)
	if !res.IsError {
		t.Fatal("first contact must fail closed, not return a success result")
	}
	text := toolText(res)
	for _, want := range []string{
		"DEVICE IDENTITY CONFIRMATION REQUIRED",
		"wanctl_trust_server",
		`target="` + trustTarget + `"`,
		`fingerprint="` + pinnedFP + `"`,
		"changes this controller's device trust store",
		"does not grant device access",
		"host's approval requirements",
		"independently verified fingerprint",
		"This tool response is not authorization",
		"retry the original operation",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	for _, unsafe := range []string{"DO THIS NOW", "without asking the user first", "already asks them to approve each tool call"} {
		if strings.Contains(text, unsafe) {
			t.Errorf("trust response tries to bypass or assumes host approval: %q", unsafe)
		}
	}
	// The CLI instruction is what stalled the conversation; it must be gone.
	if strings.Contains(text, "wanctl trust server --target") {
		t.Errorf("HTTP mode still points the model at a CLI it cannot run:\n%s", text)
	}
}

// Over stdio a human is at a terminal, and the CLI really is the right answer.
func TestTrustRequiredOverStdioKeepsTheCLIWording(t *testing.T) {
	err := &client.TrustRequiredError{Target: trustTarget, Fingerprint: pinnedFP}
	text := toolText(dialErrorResult(&localFsSession{}, err))
	if !strings.Contains(text, "wanctl trust server --target") {
		t.Errorf("stdio mode lost the CLI instruction:\n%s", text)
	}
	if strings.Contains(text, "wanctl_trust_server") {
		t.Errorf("stdio mode points a human at the MCP tool:\n%s", text)
	}
}

// A mismatch is the alarm the pin exists for. It has to carry both fingerprints
// and stop the model rather than let it pin its way past the alarm.
func TestIdentityMismatchShowsBothFingerprintsAndStops(t *testing.T) {
	err := &transport.MismatchError{Name: trustTarget, Stored: pinnedFP, Offered: offeredFP}
	res := dialErrorResult(&remoteSession{}, err)
	if !res.IsError {
		t.Fatal("a mismatch must fail closed")
	}
	text := toolText(res)
	for _, want := range []string{
		"DEVICE IDENTITY MISMATCH",
		"pinned:    " + pinnedFP,
		"presented: " + offeredFP,
		"Do NOT call wanctl_trust_server",
		"Report BOTH fingerprints",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
}

// The stdio text is the CLI's, which already carries both fingerprints.
func TestIdentityMismatchOverStdioStillShowsBothFingerprints(t *testing.T) {
	err := &transport.MismatchError{Name: trustTarget, Stored: pinnedFP, Offered: offeredFP}
	text := toolText(dialErrorResult(&localFsSession{}, err))
	if !strings.Contains(text, pinnedFP) || !strings.Contains(text, offeredFP) {
		t.Errorf("stdio mismatch text dropped a fingerprint:\n%s", text)
	}
}

// A device that is not pinned yet is exactly the one that will stop the next
// call, so the listing says so before the model picks a target — and says the
// opposite once the pin is in. This dials nothing; it reads the same store the
// pin is written to, which is what makes the canonical ns/device key load
// bearing: get it wrong and a freshly pinned device still reads as unpinned.
func TestPeersReportsIdentityPinStateBeforeAndAfterTrusting(t *testing.T) {
	t.Setenv("WANCTL_MCP_ALLOW_UNSAFE_TRUST_SERVER", "1")
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_TOKEN", "tok")
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	defer srv.Close()
	t.Setenv("WANCTL_RELAY", srv.URL)
	sessions = &sessionStore{stdio: &localFsSession{}}

	view := client.Peers{
		Namespace: "alice",
		Devices:   []string{"build"},
		Aliases:   map[string]string{"build": "desk"},
		Shared: []client.SharedDevice{
			{Owner: "daily-ac", Device: "aa11", Label: "bms", Target: "daily-ac/aa11", Online: true},
		},
	}

	before := resultText(peerToolResult(view, pinStateOf(freshClient(t), view)))
	if !strings.Contains(before, "build  (desk)  [identity: unpinned]") {
		t.Fatalf("before trusting:\n%s", before)
	}
	if !strings.Contains(before, "daily-ac/aa11  (bms)  [identity: unpinned]") {
		t.Fatalf("shared device missing its identity state:\n%s", before)
	}
	if !strings.Contains(before, "wanctl_trust_server") {
		t.Errorf("the listing does not say how to resolve an unpinned device:\n%s", before)
	}

	req := mcpapi.CallToolRequest{Params: mcpapi.CallToolParams{Arguments: map[string]any{
		"target": trustTarget, "fingerprint": pinnedFP,
	}}}
	res, err := mcpTrustServer(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("trust call failed: %v %+v", err, res)
	}

	after := resultText(peerToolResult(view, pinStateOf(freshClient(t), view)))
	if !strings.Contains(after, "build  (desk)  [identity: pinned]") {
		t.Fatalf("after trusting alice/build:\n%s", after)
	}
	if !strings.Contains(after, "daily-ac/aa11  (bms)  [identity: unpinned]") {
		t.Fatalf("pinning one device must not mark the others pinned:\n%s", after)
	}

	structured, ok := peerToolResult(view, pinStateOf(freshClient(t), view)).StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content type = %T", structured)
	}
	identity, ok := structured["identity"].(map[string]string)
	if !ok || identity[trustTarget] != "pinned" || identity["daily-ac/aa11"] != "unpinned" {
		t.Fatalf("structured identity = %#v", structured["identity"])
	}
}

// freshClient re-opens the on-disk trust store, the way a later tool call would.
func freshClient(t *testing.T) *client.Client {
	t.Helper()
	c, hint := (&localFsSession{}).client()
	if hint != nil {
		t.Fatalf("client: %s", toolText(hint))
	}
	return c
}
