package mcp

import (
	"context"
	"strings"
	"testing"

	"wanctl/internal/client"
	"wanctl/internal/transport"

	mcpapi "github.com/mark3labs/mcp-go/mcp"
)

// First contact over HTTP has to name the shortest way through it, because the
// reader is a model with no camera and no phone: the user reads nine digits off
// the device and says them out loud, and the model passes them on. Forty-three
// characters of base64 cannot travel that way — that is the complaint this
// answer exists for — so the number, the code and the device-side command all
// have to be in the text, and the code has to be described as what the device
// prints rather than something to echo back.
func TestTrustRequiredOverHTTPCarriesTheChallengeButNotTheDeviceCode(t *testing.T) {
	err := &client.TrustRequiredError{
		Target:      trustTarget,
		Fingerprint: pinnedFP,
		Number:      "482913",
	}
	res := dialErrorResult(&remoteSession{}, err)
	if !res.IsError {
		t.Fatal("first contact must fail closed")
	}
	text := toolText(res)
	for _, want := range []string{
		"DEVICE IDENTITY CONFIRMATION REQUIRED",
		"verification number: 482 913",
		"wanctl verify 482913",
		"fingerprint",
		"same refusal",
		`number="482913"`,
		"the nine digits",
		"the digits the USER read off",
		// The rules the host's approval layer made this text carry.
		"changes this controller's device trust store",
		"does not grant device access",
		"host's approval requirements",
		"This tool response is not authorization",
		"retry the original operation",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	assertNoControllerCode(t, text, deviceCodeForRefusal(t))
	if strings.Contains(text, "wanctl trust server --target") {
		t.Errorf("HTTP mode points the model at a CLI it cannot run:\n%s", text)
	}
}

// Over stdio the human is at a terminal next to the controller, so the CLI
// command is the right answer — including the device-side one they have to run
// on the other machine.
func TestTrustRequiredOverStdioCarriesBothEnds(t *testing.T) {
	err := &client.TrustRequiredError{
		Target:      trustTarget,
		Fingerprint: pinnedFP,
		Number:      "482913",
	}
	text := toolText(dialErrorResult(&localFsSession{}, err))
	for _, want := range []string{
		"wanctl verify 482913",
		"fingerprint",
		"same refusal",
		"--number 482913 --code <the code the device shows>",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	assertNoControllerCode(t, text, deviceCodeForRefusal(t))
	if strings.Contains(text, "wanctl_trust_server") {
		t.Errorf("stdio mode points a human at the MCP tool:\n%s", text)
	}
}

// A pin is a record that a human checked something. With neither the
// fingerprint nor a code read off the device there is nothing to record, so the
// handler refuses instead of trusting whatever answered.
func TestMCPTrustServerRefusesAPinNobodyVerified(t *testing.T) {
	t.Setenv("WANCTL_MCP_ALLOW_UNSAFE_TRUST_SERVER", "1")
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_TOKEN", "tok")
	t.Setenv("WANCTL_RELAY", "http://relay.invalid")
	sessions = &sessionStore{stdio: &localFsSession{}}

	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"no target", map[string]any{"code": "771204638"}, "target is required"},
		{"nothing verified", map[string]any{"target": "alice/build"}, "nothing to verify with"},
		{"code without a number", map[string]any{"target": "alice/build", "code": "771204638"}, "nothing to verify with"},
		{"number without fingerprint", map[string]any{"target": "alice/build", "number": "482913", "code": "771204638"}, "fingerprint is required with number"},
		{"code without number with fingerprint", map[string]any{"target": "alice/build", "fingerprint": pinnedFP, "code": "771204638"}, "number is required with code"},
		{"number without the code", map[string]any{"target": "alice/build", "number": "482913"}, "code is required with number"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := mcpTrustServer(context.Background(), mcpapi.CallToolRequest{
				Params: mcpapi.CallToolParams{Arguments: tc.args},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !result.IsError || !strings.Contains(toolText(result), tc.want) {
				t.Fatalf("result = %+v, want an error mentioning %q", result, tc.want)
			}
		})
	}
}

func deviceCodeForRefusal(t *testing.T) string {
	t.Helper()
	code, err := transport.VerifyCode(pinnedFP, "482913")
	if err != nil {
		t.Fatal(err)
	}
	return code
}
func assertNoControllerCode(t *testing.T, text, code string) {
	t.Helper()
	for _, forbidden := range []string{code, transport.GroupDigits(code), "this controller derived:", "verification code:"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("controller disclosed expected code %q: %s", forbidden, text)
		}
	}
}
