package agent

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"unicode"
)

// defaultDeviceName is the name an agent registers under when --name is absent.
//
// Everywhere else the hostname is a fine answer. On Android it is not: the
// hostname is hard-coded to "localhost" on every device, so a phone registers
// as "localhost" — meaningless in `wanctl peers`, and worse, two Android
// devices in one namespace collide on the same name. Asking the property
// service for the model gives a name a human recognises ("pa2353"), which is
// what the device is actually called.
func defaultDeviceName() string {
	if runtime.GOOS == "android" {
		if name := androidDeviceName(getprop); name != "" {
			return name
		}
	}
	host, _ := os.Hostname()
	// "localhost" carries no information and is what Android always reports;
	// on any platform it is a worse identifier than the generic fallback.
	if host == "" || host == "localhost" {
		return "wanctl-agent"
	}
	return host
}

// androidDeviceName derives a device name from the Android property service.
//
// ro.product.model is the marketing model ("PA2353"), present on every device.
// The vendor market-name properties are nicer when set ("Pad 3 Pro") but are
// empty on plenty of devices, so they are only a preference, never a
// requirement.
func androidDeviceName(prop func(string) string) string {
	for _, key := range []string{"ro.product.marketname", "ro.product.vendor.marketname", "ro.product.model", "ro.product.device"} {
		if name := sanitizeDeviceName(prop(key)); name != "" {
			return name
		}
	}
	return ""
}

// getprop reads one Android system property. /system/bin/getprop is the only
// interface a non-root process has to the property service, and it exists on
// every Android build.
func getprop(key string) string {
	out, err := exec.Command("getprop", key).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// sanitizeDeviceName reduces a property value to something usable as a device
// name on the wire and as a shell argument: lowercase, no spaces. Model names
// like "Pad 3 Pro" would otherwise need quoting at every `--target`.
func sanitizeDeviceName(raw string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		case unicode.IsSpace(r):
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-._")
}
