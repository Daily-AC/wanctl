package sessionauth

import (
	"encoding/json"
	"testing"
)

// A shared session carries what the owner's does. There is no reduced grant set
// to parse any more, so nothing may quietly reintroduce one.
func TestFullCapabilitiesCoversEveryCapability(t *testing.T) {
	for _, item := range capabilityNames {
		if !FullCapabilities.Has(item.cap) {
			t.Fatalf("FullCapabilities is missing %q", item.name)
		}
	}
	if got := FullCapabilities.String(); got != "exec,read,write,logs,console" {
		t.Fatalf("FullCapabilities = %q", got)
	}
	for _, invalid := range []string{"", "read,unknown", "read,read"} {
		if _, err := Parse(invalid); err == nil {
			t.Errorf("Parse(%q) succeeded", invalid)
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
