package agent

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestAndroidDeviceNamePrefersMarketNameThenModel(t *testing.T) {
	props := map[string]string{"ro.product.marketname": "Pad 3 Pro\n", "ro.product.model": "PA2353\n"}
	if got := androidDeviceName(func(k string) string { return props[k] }); got != "pad-3-pro" {
		t.Fatalf("got %q want %q", got, "pad-3-pro")
	}
}

// The tablet this was verified on: market-name properties empty, model set.
func TestAndroidDeviceNameFallsBackToModel(t *testing.T) {
	props := map[string]string{"ro.product.marketname": "", "ro.product.vendor.marketname": "", "ro.product.model": "PA2353\n", "ro.product.device": "DPD2305\n"}
	if got := androidDeviceName(func(k string) string { return props[k] }); got != "pa2353" {
		t.Fatalf("got %q want %q", got, "pa2353")
	}
}

// A device whose property service tells us nothing must not produce a name made
// of leftover punctuation.
func TestAndroidDeviceNameEmptyWhenNoPropertyIsUsable(t *testing.T) {
	if got := androidDeviceName(func(string) string { return "  \n" }); got != "" {
		t.Fatalf("got %q want empty", got)
	}
}

func TestSanitizeDeviceName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"PA2353", "pa2353"},
		{"Pad 3 Pro", "pad-3-pro"},
		{"Pixel_7-Pro", "pixel_7-pro"},
		{"  spaced  ", "spaced"},
		{"!!!", ""},
		{"redmi/note(12)", "redminote12"},
	} {
		if got := sanitizeDeviceName(tc.in); got != tc.want {
			t.Errorf("sanitizeDeviceName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Platforms without a better source keep whatever hostname they had, including
// "localhost". The relay promotes a legacy record by certificate fingerprint, so
// the label may change freely, but changing it for no reason is still noise.
func TestDefaultDeviceNameLeavesPlainHostnamesAlone(t *testing.T) {
	if runtime.GOOS == "android" || runtime.GOOS == "darwin" {
		t.Skip("this asserts the platforms with no better name source")
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Skip("no hostname to compare against")
	}
	if got := defaultDeviceName(); got != host {
		t.Fatalf("defaultDeviceName() = %q, want the hostname %q", got, host)
	}
}

// The bug this fixes: os.Hostname() on this Mac reads "bogon" behind a router
// with no reverse DNS and "localhost" on a bare boot, while the local host name
// stays "zyldeMacBook-Pro" across both.
func TestDarwinDeviceNamePrefersLocalHostName(t *testing.T) {
	if got := darwinDeviceName(func() string { return "zyldeMacBook-Pro\n" }); got != "zyldeMacBook-Pro" {
		t.Fatalf("got %q want %q", got, "zyldeMacBook-Pro")
	}
}

// scutil exits non-zero (and prints nothing) when no local host name is set;
// the caller must fall back rather than register an empty label.
func TestDarwinDeviceNameEmptyWhenUnset(t *testing.T) {
	for _, in := range []string{"", "  \n", "name/with-slash", "ctrl\x01char", strings.Repeat("x", 256)} {
		if got := darwinDeviceName(func() string { return in }); got != "" {
			t.Fatalf("darwinDeviceName(%q) = %q, want empty", in, got)
		}
	}
}

// On a real Mac the two sources disagree, and the agent must take the stable one.
func TestDefaultDeviceNameOnDarwinUsesLocalHostName(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("this asserts the darwin path")
	}
	want := darwinDeviceName(scutilLocalHostName)
	if want == "" {
		host, _ := os.Hostname()
		want = host
	}
	if got := defaultDeviceName(); got != want {
		t.Fatalf("defaultDeviceName() = %q, want %q", got, want)
	}
}
