package catalog

import (
	"strings"
	"testing"
)

// The code flow must retain the identity observed before its number was
// disclosed. Keep that binding visible in both the schema and usage examples.
func TestTrustCodeContractRequiresOriginalFingerprint(t *testing.T) {
	trust, ok := Lookup("wanctl_trust_server")
	if !ok {
		t.Fatal("trust server is missing from the catalog")
	}
	var fingerprint *Param
	for i := range trust.Params {
		if trust.Params[i].Name == "fingerprint" {
			fingerprint = &trust.Params[i]
			break
		}
	}
	if fingerprint == nil || !fingerprint.Required {
		t.Fatal("fingerprint must be required in the trust server schema")
	}
	for _, phrase := range []string{
		"SAME refusal",
		"never supplies the expected code",
		"ON THE DEVICE",
		"All four are required",
		"Do not derive, invent or reuse a code",
	} {
		if !strings.Contains(trust.Desc, phrase) {
			t.Errorf("trust instructions omit %q", phrase)
		}
	}
	for _, flag := range []string{"--target", "--fingerprint", "--number", "--code"} {
		if !strings.Contains(trust.CLIExample, flag) {
			t.Errorf("CLI code example omits %s", flag)
		}
	}
	for _, param := range []string{"target", "fingerprint", "number", "code"} {
		if !strings.Contains(trust.MCPExample, `"`+param+`":`) {
			t.Errorf("MCP code example omits %s", param)
		}
	}
}
