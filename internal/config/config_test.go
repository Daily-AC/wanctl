package config

import "testing"

func TestProductionDefaults(t *testing.T) {
	if DefaultRelay != "https://wanctl-relay.***REMOVED***.***REMOVED***.com" {
		t.Fatalf("DefaultRelay = %q", DefaultRelay)
	}
	if DefaultTransport != "http" {
		t.Fatalf("DefaultTransport = %q", DefaultTransport)
	}
}
