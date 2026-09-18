package server

import (
	"reflect"
	"testing"
	"time"
)

// The process-table walk is the part of session cancellation that is the same
// on every OS, so it is tested on every OS — including Windows, where the
// snapshot itself cannot be exercised from here.
func TestDescendantsTakesTheWholeSubtreeDeepestFirst(t *testing.T) {
	//	100 (the session shell)
	//	├── 200 ── 300 ── 400
	//	└── 210
	//	900 (unrelated) ── 910
	parents := map[int]int{
		1: 0, 100: 1, 200: 100, 210: 100, 300: 200, 400: 300, 900: 1, 910: 900,
	}
	got := descendants(parents, 100)
	want := []int{400, 300, 200, 210}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("descendants = %v, want %v (deepest first, shell and strangers excluded)", got, want)
	}
	for _, pid := range got {
		if pid == 100 {
			t.Fatal("the session shell is in its own kill list; cancelling would destroy the session")
		}
	}
}

func TestDescendantsOfALeafIsEmpty(t *testing.T) {
	parents := map[int]int{1: 0, 100: 1, 200: 1}
	if got := descendants(parents, 100); len(got) != 0 {
		t.Fatalf("descendants of a childless shell = %v, want none", got)
	}
}

// A snapshot taken while processes are exiting can contain a row whose parent
// has already been reused. The walk must not spin on it.
func TestDescendantsSurvivesACycle(t *testing.T) {
	parents := map[int]int{100: 1, 200: 100, 300: 200, 400: 500, 500: 400}
	done := make(chan []int, 1)
	go func() { done <- descendants(parents, 100) }()
	select {
	case got := <-done:
		want := []int{300, 200}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("descendants = %v, want %v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("descendants did not terminate on a snapshot containing a cycle")
	}
}

func TestParseProcessTableAcceptsEveryPsWeRunOn(t *testing.T) {
	cases := map[string]string{
		"macOS": "  PID  PPID\n    1     0\n  518     1\n 4210   518\n",
		// procps pads differently and can be asked for no header at all.
		"procps":  "    1     0\n 1420     1\n 1500  1420\n",
		"toybox":  "PID PPID\n1 0\n518 1\n4210 518\n",
		"trailer": "  PID  PPID\n    1     0\n  518     1\n 4210   518\n\n",
	}
	want := map[string]map[int]int{
		"macOS":   {1: 0, 518: 1, 4210: 518},
		"procps":  {1: 0, 1420: 1, 1500: 1420},
		"toybox":  {1: 0, 518: 1, 4210: 518},
		"trailer": {1: 0, 518: 1, 4210: 518},
	}
	for name, table := range cases {
		got := parseProcessTable(table)
		if !reflect.DeepEqual(got, want[name]) {
			t.Fatalf("%s: parsed %v, want %v", name, got, want[name])
		}
	}
}

func TestParseProcessTableIgnoresUnreadableRows(t *testing.T) {
	got := parseProcessTable("PID PPID\nnot a row\n42 7\n<defunct>\n")
	if !reflect.DeepEqual(got, map[int]int{42: 7}) {
		t.Fatalf("parsed %v, want just the one readable row", got)
	}
}
