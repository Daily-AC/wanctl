//go:build !windows

package release

// OSMajorVersion is only meaningful on Windows; see ArtifactArch.
func OSMajorVersion() uint32 { return 0 }
