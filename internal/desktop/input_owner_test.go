package desktop

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func TestInputSourceSecondHelperMarkerIsForeign(t *testing.T) {
	other := uintptr(ownInputMarker) ^ 1 // a different helper's marker
	for _, kind := range []uint32{0, 1} {
		for _, origin := range []uint32{0, 1, 2, 4} {
			if rawDeviceInput(kind, 0, origin, ownInputMarker) {
				t.Errorf("own input stopped batch: kind=%d origin=%d", kind, origin)
			}
			if !rawDeviceInput(kind, 0, origin, other) {
				t.Errorf("second helper's input was ignored: kind=%d origin=%d", kind, origin)
			}
		}
	}
}

func TestInputSourceMarkerIsPerHelperProcess(t *testing.T) {
	// A helper invocation is a fresh process. This child only reports package
	// initialization state; it never starts a native observer or sends input.
	const childEnv = "WANCTL_TEST_INPUT_MARKER_CHILD"
	if os.Getenv(childEnv) == "1" {
		fmt.Printf("input-marker:%x\n", ownInputMarker)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestInputSourceMarkerIsPerHelperProcess$")
	cmd.Env = append(os.Environ(), childEnv+"=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("marker-only child failed: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "input-marker:") {
			continue
		}
		other, err := strconv.ParseUint(strings.TrimPrefix(line, "input-marker:"), 16, 64)
		if err != nil || other == 0 || ownInputMarker == 0 || uintptr(other) == ownInputMarker {
			t.Fatal("separate helper processes must have distinct, nonzero input markers")
		}
		if !rawDeviceInput(1, 0, 2, uintptr(other)) || rawDeviceInput(1, 0, 2, ownInputMarker) {
			t.Fatal("foreign child marker and own marker were not distinguished")
		}
		return
	}
	t.Fatal("marker-only child did not return its marker")
}
