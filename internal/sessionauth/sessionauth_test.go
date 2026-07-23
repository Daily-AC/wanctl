package sessionauth

import (
	"encoding/json"
	"testing"
)

func TestParseGrant(t *testing.T) {
	caps, err := ParseGrant("write, read,exec")
	if err != nil {
		t.Fatal(err)
	}
	if caps != GrantCapabilities {
		t.Fatalf("capabilities = %q", caps)
	}
	for _, invalid := range []string{"", "read,unknown", "read,read", "logs", "console"} {
		if _, err := ParseGrant(invalid); err == nil {
			t.Errorf("ParseGrant(%q) succeeded", invalid)
		}
	}
}

func TestCapabilitiesJSONRoundTrip(t *testing.T) {
	want := Read | Exec
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"exec,read"` {
		t.Fatalf("JSON = %s", b)
	}
	var got Capabilities
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round trip = %q, want %q", got, want)
	}
	if err := json.Unmarshal([]byte(`"read,unknown"`), &got); err == nil {
		t.Fatal("unknown wire capability was accepted")
	}
}
