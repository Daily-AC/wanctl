package androiddns

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"reflect"
	"testing"
)

// fakeFS answers readFile from a map; anything absent behaves like a device
// that does not have the file, which is the whole point on Android.
func fakeFS(files map[string]string) func(string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		if body, ok := files[name]; ok {
			return []byte(body), nil
		}
		return nil, fs.ErrNotExist
	}
}

func env(pairs map[string]string) func(string) string {
	return func(k string) string { return pairs[k] }
}

func TestNameserversPrefersExplicitOverride(t *testing.T) {
	got := Nameservers(
		env(map[string]string{EnvVar: "10.0.0.53, 10.0.0.54:5353"}),
		fakeFS(map[string]string{"/etc/resolv.conf": "nameserver 1.2.3.4\n"}),
	)
	want := []string{"10.0.0.53:53", "10.0.0.54:5353"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("override ignored: got %v want %v", got, want)
	}
}

// A device that does have /etc/resolv.conf needs no help: the stock resolver
// reads it correctly, so we must leave DefaultResolver alone.
func TestNameserversLeavesStockResolverAloneWhenResolvConfExists(t *testing.T) {
	got := Nameservers(env(nil), fakeFS(map[string]string{"/etc/resolv.conf": "nameserver 1.2.3.4\n"}))
	if got != nil {
		t.Fatalf("expected nil (no change), got %v", got)
	}
}

func TestNameserversReadsTermuxResolvConf(t *testing.T) {
	prefix := "/data/data/com.termux/files/usr"
	got := Nameservers(
		env(map[string]string{"PREFIX": prefix}),
		fakeFS(map[string]string{prefix + "/etc/resolv.conf": "# termux\nnameserver 8.8.4.4\nnameserver 2001:4860:4860::8844\nsearch lan\n"}),
	)
	want := []string{"8.8.4.4:53", "[2001:4860:4860::8844]:53"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// The Android 16 probe: no /etc/resolv.conf, no Termux prefix, netd invisible.
// Without a fallback the binary cannot resolve its own relay.
func TestNameserversFallsBackWhenDeviceOffersNothing(t *testing.T) {
	got := Nameservers(env(nil), fakeFS(nil))
	if !reflect.DeepEqual(got, FallbackNameservers) {
		t.Fatalf("got %v want %v", got, FallbackNameservers)
	}
}

// An empty or malformed Termux resolv.conf must not leave the device with zero
// nameservers — that would be the "connection refused" failure all over again.
func TestNameserversFallsBackOnUnusableTermuxResolvConf(t *testing.T) {
	prefix := "/data/data/com.termux/files/usr"
	got := Nameservers(
		env(map[string]string{"PREFIX": prefix}),
		fakeFS(map[string]string{prefix + "/etc/resolv.conf": "# nothing useful here\noptions ndots:1\n"}),
	)
	if !reflect.DeepEqual(got, FallbackNameservers) {
		t.Fatalf("got %v want %v", got, FallbackNameservers)
	}
}

// A retry must land on a different operator, otherwise a blocked or hijacked
// resolver takes the device offline no matter how many attempts Go makes.
func TestResolverRotatesServersSoRetriesReachADifferentOperator(t *testing.T) {
	var dialed []string
	stop := errors.New("not dialing in a test")
	r := resolverWith([]string{"a:53", "b:53", "c:53"}, func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		return nil, stop
	})
	if r == nil {
		t.Fatal("expected a resolver")
	}
	for i := 0; i < 4; i++ {
		if _, err := r.Dial(context.Background(), "udp", "ignored"); !errors.Is(err, stop) {
			t.Fatalf("dial %d: unexpected error %v", i, err)
		}
	}
	want := []string{"a:53", "b:53", "c:53", "a:53"}
	if !reflect.DeepEqual(dialed, want) {
		t.Fatalf("rotation broken: got %v want %v", dialed, want)
	}
}

func TestResolverIsNilWithoutServers(t *testing.T) {
	if r := Resolver(nil); r != nil {
		t.Fatalf("expected nil resolver, got %v", r)
	}
}
