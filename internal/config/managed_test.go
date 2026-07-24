package config

import "testing"

func TestManagedPIDIsOwnedByRecordedProcess(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	if got := ManagedPID(); got != 0 {
		t.Fatalf("initial managed pid = %d", got)
	}
	if err := WriteManagedPID(41); err != nil {
		t.Fatal(err)
	}
	if err := RemoveManagedPID(42); err != nil {
		t.Fatal(err)
	}
	if got := ManagedPID(); got != 41 {
		t.Fatalf("marker removed by wrong pid; got %d", got)
	}
	if err := RemoveManagedPID(41); err != nil {
		t.Fatal(err)
	}
	if got := ManagedPID(); got != 0 {
		t.Fatalf("managed pid after owner cleanup = %d", got)
	}
}
