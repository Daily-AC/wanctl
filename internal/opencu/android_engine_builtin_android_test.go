package opencu

import "testing"

func TestAndroidBuildCarriesTheEngine(t *testing.T) {
	if loadBuiltInAndroidEngine() == nil {
		t.Fatal("the built-in engine does not match its manifest")
	}
}
