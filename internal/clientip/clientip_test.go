package clientip

import (
	"net/http/httptest"
	"testing"
)

func TestKey(t *testing.T) {
	for _, tc := range []struct {
		name    string
		remote  string
		headers map[string]string
		want    string
	}{
		{"direct client", "203.0.113.9:40000", nil, "203.0.113.9"},
		{"direct client cannot rename itself", "203.0.113.9:40000",
			map[string]string{"X-Real-IP": "198.51.100.1"}, "203.0.113.9"},
		{"proxy on loopback names the client", "127.0.0.1:51000",
			map[string]string{"X-Real-IP": "198.51.100.1"}, "198.51.100.1"},
		{"proxy behind a docker port mapping", "172.18.0.1:51000",
			map[string]string{"X-Real-IP": "198.51.100.1"}, "198.51.100.1"},
		{"proxy on an IPv6 loopback", "[::1]:51000",
			map[string]string{"X-Real-IP": "198.51.100.1"}, "198.51.100.1"},
		{"proxy that sent no X-Real-IP is itself the client", "127.0.0.1:51000", nil, "127.0.0.1"},
		{"garbage X-Real-IP is ignored", "127.0.0.1:51000",
			map[string]string{"X-Real-IP": "not-an-address"}, "127.0.0.1"},
		{"X-Forwarded-For never names the client", "127.0.0.1:51000",
			map[string]string{"X-Forwarded-For": "198.51.100.1"}, "127.0.0.1"},
		{"X-Real-IP wins over X-Forwarded-For", "127.0.0.1:51000",
			map[string]string{"X-Real-IP": "198.51.100.1", "X-Forwarded-For": "192.0.2.7, 198.51.100.1"}, "198.51.100.1"},
		{"IPv4-mapped address is the IPv4 client", "127.0.0.1:51000",
			map[string]string{"X-Real-IP": "::ffff:198.51.100.1"}, "198.51.100.1"},
		{"IPv6 client is its /64", "127.0.0.1:51000",
			map[string]string{"X-Real-IP": "2001:db8:1:2:aaaa::1"}, "2001:db8:1:2::/64"},
		{"direct IPv6 client is its /64", "[2001:db8:1:2:bbbb::7]:40000", nil, "2001:db8:1:2::/64"},
		{"not an IP peer", "@", nil, "@"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = tc.remote
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			if got := Key(req); got != tc.want {
				t.Fatalf("Key = %q, want %q", got, tc.want)
			}
		})
	}
}

// Two hosts on one /64 are one subscriber; the next /64 over is somebody else.
func TestKeyGroupsIPv6BySubscriberPrefix(t *testing.T) {
	key := func(addr string) string {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "127.0.0.1:51000"
		req.Header.Set("X-Real-IP", addr)
		return Key(req)
	}
	if key("2001:db8:0:1::1") != key("2001:db8:0:1:ffff::2") {
		t.Fatal("two addresses in one /64 got different keys")
	}
	if key("2001:db8:0:1::1") == key("2001:db8:0:2::1") {
		t.Fatal("addresses in different /64s share a key")
	}
}
