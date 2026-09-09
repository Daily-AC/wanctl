package agent

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"unicode"
)

// defaultDeviceName supplies a human-readable label when --name is absent.
// Android's hostname is usually localhost, so use its product model instead.
// Device identity and routing are independent of this label.
func defaultDeviceName() string {
	switch runtime.GOOS {
	case "android":
		if name := androidDeviceName(getprop); name != "" {
			return name
		}
	case "darwin":
		if name := darwinDeviceName(scutilLocalHostName); name != "" {
			return name
		}
	}
	host, _ := os.Hostname()
	return host
}

// darwinDeviceName prefers the macOS local host name over the kernel hostname.
//
// os.Hostname() on macOS returns the kernel hostname, which the system rewrites
// from whatever the current network hands it: it reads "localhost" on a bare
// boot and "bogon" behind a router with no reverse DNS, and it changes at
// runtime when the machine moves between networks. The local host name (the
// Bonjour name, System Settings > General > Sharing) is user-set and stays put.
//
// The label alone no longer decides identity - the relay promotes a legacy
// record by certificate fingerprint - but a name that changes on every DHCP
// lease is still useless in `wanctl peers` output and in `--target`.
func darwinDeviceName(localHostName func() string) string {
	name := strings.TrimSpace(localHostName())
	if name == "" || len(name) > 255 || strings.Contains(name, "/") {
		return ""
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return ""
		}
	}
	return name
}

// scutilLocalHostName reads the local host name from the system configuration
// database. The absolute path is deliberate, for the same reason getprop uses
// one: a name lookup can find some other binary on PATH.
const scutilPath = "/usr/sbin/scutil"

func scutilLocalHostName() string {
	out, err := exec.Command(scutilPath, "--get", "LocalHostName").Output()
	if err != nil {
		return ""
	}
	return string(out)
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
//
// The absolute path is required, not tidiness. Termux ships its own getprop and
// puts $PREFIX/bin ahead of /system/bin on PATH, so resolving by name finds a
// binary inside the app's private data directory — which Android refuses to let
// this process exec (the same rule that decides the session shell, see
// server.androidShell). Resolving by name therefore failed on Termux and every
// device fell back to the generic name; /system/bin/getprop answers fine.
const getpropPath = "/system/bin/getprop"

func getprop(key string) string {
	out, err := exec.Command(getpropPath, key).Output()
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
