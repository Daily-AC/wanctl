//go:build windows

package release

import "golang.org/x/sys/windows"

// osMajorVersion asks RtlGetVersion because it is not subject to the
// application compatibility shim. GetVersionEx, and anything built on it,
// reports 6.2 on Windows 10 and later to an executable whose manifest does not
// declare them, and a Go binary carries no manifest — which would put every
// Windows 10 machine on the Windows 7 build.
func osMajorVersion() uint32 { return windows.RtlGetVersion().MajorVersion }
