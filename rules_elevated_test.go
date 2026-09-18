package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"wanctl/internal/policy"
	"wanctl/internal/script"
)

// `wanctl rules add` refused --kind exec-elevated, which is the whole of #63:
// the policy engine has always understood the class, the approval UI did not
// offer it, and the CLI would not create it — so an elevated command on an
// unattended phone could not be pre-authorized anywhere.
func TestRulesAddAcceptsExecElevated(t *testing.T) {
	bin := buildWanctl(t)
	dir := t.TempDir()
	run := func(args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "WANCTL_CONFIG_DIR="+dir, "WANCTL_RELAY=", "WANCTL_TOKEN=")
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("run %v: %v", args, err)
		}
		return string(out), code
	}

	if out, code := run("rules", "add", "--kind", "exec-elevated", "--pattern", "pm install *"); code != 0 {
		t.Fatalf("rules add --kind exec-elevated failed (%d): %s", code, out)
	}
	out, code := run("rules")
	if code != 0 {
		t.Fatalf("rules list failed (%d): %s", code, out)
	}
	if !strings.Contains(out, "exec-elevated") || !strings.Contains(out, "pm install *") {
		t.Fatalf("the rule is not in the list:\n%s", out)
	}

	// Persisted where the agent reads it, and matching as an elevated rule.
	t.Setenv("WANCTL_CONFIG_DIR", dir)
	eng, err := policy.Open("rules.json", policy.ModeNormal)
	if err != nil {
		t.Fatal(err)
	}
	if !eng.Allowed(policy.Request{Kind: policy.KindExecElevated, Cmd: "pm install -r /sdcard/app.apk"}) {
		t.Fatal("the persisted exec-elevated rule did not authorize the command it names")
	}
	if eng.Allowed(policy.Request{Kind: policy.KindExec, Cmd: "pm install -r /sdcard/app.apk"}) {
		t.Fatal("an exec-elevated rule authorized the unelevated form")
	}
	if _, err := os.Stat(filepath.Join(dir, "rules.json")); err != nil {
		t.Fatalf("rules.json: %v", err)
	}
}

// The same, for a command that was sent with --script: the pattern is the
// token the device names the script by, not the base64 transport.
func TestRulesAddAcceptsAScriptToken(t *testing.T) {
	bin := buildWanctl(t)
	dir := t.TempDir()
	cmd, err := script.Command(script.POSIX, []byte("pm install -r /sdcard/Download/app.apk\n"))
	if err != nil {
		t.Fatal(err)
	}
	tok, ok := script.Canonical(cmd)
	if !ok {
		t.Fatal("a generated script command was not recognised as one")
	}

	add := exec.Command(bin, "rules", "add", "--kind", "exec-elevated", "--pattern", tok)
	add.Env = append(os.Environ(), "WANCTL_CONFIG_DIR="+dir, "WANCTL_RELAY=", "WANCTL_TOKEN=")
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("rules add: %v\n%s", err, out)
	}

	t.Setenv("WANCTL_CONFIG_DIR", dir)
	eng, err := policy.Open("rules.json", policy.ModeNormal)
	if err != nil {
		t.Fatal(err)
	}
	if !eng.Allowed(policy.Request{Kind: policy.KindExecElevated, Cmd: cmd}) {
		t.Fatalf("a rule added as %q did not match the script it names", tok)
	}
}

func TestRulesAddStillRejectsAnUnknownKind(t *testing.T) {
	bin := buildWanctl(t)
	c := exec.Command(bin, "rules", "add", "--kind", "root", "--pattern", "x")
	c.Env = append(os.Environ(), "WANCTL_CONFIG_DIR="+t.TempDir(), "WANCTL_RELAY=", "WANCTL_TOKEN=")
	out, err := c.CombinedOutput()
	if err == nil {
		t.Fatalf("an unknown kind was accepted:\n%s", out)
	}
	if !strings.Contains(string(out), "exec-elevated") {
		t.Fatalf("the error does not list the kinds that exist:\n%s", out)
	}
}
