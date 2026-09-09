package main

import "testing"

// `wanctl share manage --device DEV --to NS on|off` is how an owner changes
// their mind about a share without revoking it. The switch is a word, not a
// flag, so there is one spelling for each position and no way to write both.
func TestParseShareManage(t *testing.T) {
	for _, tc := range []struct {
		name           string
		args           []string
		device, to     string
		manage, wantOK bool
	}{
		{name: "on", args: []string{"--device", "bms", "--to", "waerjili123", "on"}, device: "bms", to: "waerjili123", manage: true, wantOK: true},
		{name: "off", args: []string{"--device", "bms", "--to", "waerjili123", "off"}, device: "bms", to: "waerjili123", wantOK: true},
		{name: "flags after the word", args: []string{"on", "--device", "bms", "--to", "ns"}},
		{name: "no word", args: []string{"--device", "bms", "--to", "ns"}},
		{name: "two words", args: []string{"--device", "bms", "--to", "ns", "on", "off"}},
		{name: "not a position", args: []string{"--device", "bms", "--to", "ns", "yes"}},
		{name: "no device", args: []string{"--to", "ns", "on"}},
		{name: "no grantee", args: []string{"--device", "bms", "on"}},
		{name: "nothing", args: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			device, to, manage, err := parseShareManage(tc.args)
			if (err == nil) != tc.wantOK {
				t.Fatalf("err = %v, want ok=%v", err, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if device != tc.device || to != tc.to || manage != tc.manage {
				t.Fatalf("parsed device=%q to=%q manage=%v", device, to, manage)
			}
		})
	}
}

// Both listings name the one thing a share varies, so an owner can read off
// which of their shares can administer the device.
func TestManageLabelNamesBothPositions(t *testing.T) {
	if manageLabel(true) == manageLabel(false) {
		t.Fatal("the two positions must not print the same")
	}
	if manageLabel(true) == "" || manageLabel(false) == "" {
		t.Fatal("a share's management state is never blank")
	}
}
