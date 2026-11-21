package release

import "runtime"

// Windows7Arch is the pseudo-architecture of the windows/amd64 build that
// still starts on Windows 7 and 8.x: "wanctl-windows-amd64.win7.exe".
//
// Go 1.21 dropped those systems, and every Windows binary it links since is
// stamped for Windows 10, which the older loader refuses to start at all. So
// that build comes from a different toolchain (scripts/go-legacy-win7.sh) and
// is a second artifact for the real platform windows/amd64. It rides in the
// manifest the way the APKs do (see APKArch), for the same reasons: no schema
// change, the Name == ArtifactName(OS, Arch) rule still holds, and no Go
// toolchain reports GOARCH as "amd64.win7", so older clients never select it.
const Windows7Arch = "amd64.win7"

// ArtifactArch is the manifest arch under which a binary for goos/goarch,
// running on an OS whose major version is osMajor, finds its own update.
// osMajor only matters on Windows; elsewhere pass anything.
//
// There is deliberately no fallback between the two windows/amd64 builds. A
// Windows 7 machine asking a release with no amd64.win7 entry (every release
// before the build existed) gets "no artifact" and keeps what it runs: the
// plain build would install cleanly and then never start again. 32-bit and
// ARM Windows below 10 have no build that starts there, and nothing running
// wanctl on such a machine can exist to ask, so their arch is left alone.
func ArtifactArch(goos, goarch string, osMajor uint32) string {
	if goos == "windows" && goarch == "amd64" && osMajor < 10 {
		return Windows7Arch
	}
	return goarch
}

// HostArtifactArch is ArtifactArch for the running binary on this machine.
// Everything that fetches this binary's own successor goes through it.
func HostArtifactArch() string {
	return ArtifactArch(runtime.GOOS, runtime.GOARCH, OSMajorVersion())
}
