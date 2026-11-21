//go:build !windows

package release

// osMajorVersion is only consulted for Windows; see ArtifactArch.
func osMajorVersion() uint32 { return 0 }
