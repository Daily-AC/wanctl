package agent

import "testing"

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
