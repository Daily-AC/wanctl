package main

import (
	"os/exec"
	"testing"
)

// TestBuilds ensures the whole module compiles and vets cleanly.
func TestBuilds(t *testing.T) {
	if out, err := exec.Command("go", "build", "./...").CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	if out, err := exec.Command("go", "vet", "./...").CombinedOutput(); err != nil {
		t.Fatalf("vet failed: %v\n%s", err, out)
	}
}
