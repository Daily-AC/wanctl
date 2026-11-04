package opencu

import _ "embed"

// The official OpenCU Android engine ships inside the signed APK, so first use
// needs no network and no new trust: GitHub may be unreachable from the phone,
// and a failed download counts against the 24-hour refresh interval. To update
// it, replace both files with the assets of an OpenCU release, unchanged.
//
//go:embed androidengine/opencu-android.json
var builtInAndroidManifest []byte

//go:embed androidengine/opencu-android.jar
var builtInAndroidJar string

// A bad pair is caught by TestBuiltInAndroidEngineMatchesItsManifest. If one
// ever shipped, the agent would still fall back to downloading.
func loadBuiltInAndroidEngine() *builtInAndroidEngine {
	engine, err := parseBuiltInAndroidEngine(builtInAndroidManifest, builtInAndroidJar)
	if err != nil {
		return nil
	}
	return engine
}
