package relay

import "testing"

func TestDeriveNS(t *testing.T) {
	cases := map[string]string{
		"***REMOVED***@***REMOVED***": "***REMOVED***",
		"Alice.Bo@x.com":            "alice-bo",
		"bob":                       "bob",
		"ou_abc123":                 "ou-abc123",
	}
	for in, want := range cases {
		if got := deriveNS(in); got != want {
			t.Errorf("deriveNS(%q)=%q want %q", in, got, want)
		}
	}
}

func TestHashTokenStable(t *testing.T) {
	if HashToken("x") != HashToken("x") {
		t.Fatal("hash not stable")
	}
	if HashToken("a") == HashToken("b") {
		t.Fatal("hash collision")
	}
}
