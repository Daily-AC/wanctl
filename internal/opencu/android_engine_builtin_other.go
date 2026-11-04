//go:build !android

package opencu

// Only the APK's wanctl runs the Android engine, so other builds do not carry it.
func loadBuiltInAndroidEngine() *builtInAndroidEngine { return nil }
