package androidverb

import (
	"context"
	"io"
	"strings"
	"testing"

	"wanctl/internal/elevate"
)

// recorder captures the shell command a verb produced.
type recorder struct {
	cmd  string
	via  elevate.Kind
	code int
}

func (r *recorder) Run(_ context.Context, via elevate.Kind, command, _ string, out io.Writer) (elevate.Kind, int, error) {
	r.cmd = command
	r.via = via
	io.WriteString(out, "ok")
	return elevate.KindSu, r.code, nil
}

func dispatch(t *testing.T, command string) (*recorder, bool, error) {
	t.Helper()
	r := &recorder{}
	handled, _, _, err := Dispatch(context.Background(), command, "", r, io.Discard)
	return r, handled, err
}

func TestOrdinaryCommandsArePassedThrough(t *testing.T) {
	// The elevation channel already gives the whole adb surface; a verb layer
	// that swallowed unknown commands would take that away.
	for _, cmd := range []string{"pm list packages", "id", "dumpsys battery", "echo screenshot"} {
		r, handled, err := dispatch(t, cmd)
		if handled {
			t.Fatalf("%q was claimed as a verb (ran %q)", cmd, r.cmd)
		}
		if err != nil {
			t.Fatalf("%q: %v", cmd, err)
		}
	}
}

func TestScreenshotProducesScreencap(t *testing.T) {
	r, handled, err := dispatch(t, "screenshot")
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if r.cmd != "/system/bin/screencap -p" {
		t.Fatalf("cmd = %q", r.cmd)
	}
}

func TestScreenshotRefusesArgumentsRatherThanIgnoringThem(t *testing.T) {
	// `screenshot -o x.png` is what a person types after reading the wrapper's
	// help. Silently ignoring -o would write the PNG to their terminal.
	_, handled, err := dispatch(t, "screenshot -o /tmp/a.png")
	if !handled {
		t.Fatal("screenshot with arguments was passed through as a shell command")
	}
	if err == nil {
		t.Fatal("screenshot accepted arguments it does not implement")
	}
	if !strings.Contains(err.Error(), "wanctl screenshot") {
		t.Fatalf("error = %q, want it to point at the controller-side wrapper", err)
	}
}

func TestInputBuildsAndValidates(t *testing.T) {
	ok := []struct{ in, want string }{
		{"input tap 100 200", "/system/bin/input 'tap' '100' '200'"},
		{"input swipe 1 2 3 4", "/system/bin/input 'swipe' '1' '2' '3' '4'"},
		{"input swipe 1 2 3 4 300", "/system/bin/input 'swipe' '1' '2' '3' '4' '300'"},
		{"input key KEYCODE_HOME", "/system/bin/input 'key' 'KEYCODE_HOME'"},
	}
	for _, c := range ok {
		r, handled, err := dispatch(t, c.in)
		if !handled || err != nil {
			t.Fatalf("%q: handled=%v err=%v", c.in, handled, err)
		}
		if r.cmd != c.want {
			t.Fatalf("%q built %q, want %q", c.in, r.cmd, c.want)
		}
	}

	bad := []string{
		"input",
		"input tap",
		"input tap 100",
		"input tap x y",      // non-numeric coordinates reach `input` as a confusing error
		"input swipe 1 2 3",  // too few
		"input frobnicate 1", // unknown subcommand
		"input key",          // missing keycode
	}
	for _, c := range bad {
		_, handled, err := dispatch(t, c)
		if !handled {
			t.Fatalf("%q was passed through instead of refused", c)
		}
		if err == nil {
			t.Fatalf("%q was accepted", c)
		}
	}
}

// TestInputTextKeepsOneArgument is the case the verb exists for: a string with
// a space in it must reach `input text` as one argument.
func TestInputTextKeepsOneArgument(t *testing.T) {
	r, handled, err := dispatch(t, `input text "hello world"`)
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if r.cmd != `/system/bin/input 'text' 'hello world'` {
		t.Fatalf("cmd = %q", r.cmd)
	}
}

// reparse runs the generated command back through the shell grammar and
// returns its argv. It fails the test if the command turned out to be anything
// other than one simple command — which is the actual property that matters
// when the result is about to run as root.
func reparse(t *testing.T, command string) []string {
	t.Helper()
	argv, ok := splitArgs(command)
	if !ok {
		t.Fatalf("generated command is not a single simple literal command: %q", command)
	}
	return argv
}

// TestArgumentsAreQuotedAgainstInjection is the security case. These commands
// are about to run as root, and a package name or a settings value is user
// data; it must arrive as one argv slot rather than as a second command.
func TestArgumentsAreQuotedAgainstInjection(t *testing.T) {
	cases := []struct {
		in       string
		wantArgv []string
	}{
		{`app uninstall "com.x; rm -rf /"`,
			[]string{"/system/bin/pm", "uninstall", "com.x; rm -rf /"}},
		{`app uninstall "com.x' ; id ; '"`,
			[]string{"/system/bin/pm", "uninstall", "com.x' ; id ; '"}},
		{`settings put global k "v; reboot"`,
			[]string{"/system/bin/settings", "put", "global", "k", "v; reboot"}},
		{`input text "a'b"`,
			[]string{"/system/bin/input", "text", "a'b"}},
		// A literal $(...) — single-quoted, so it arrived as data rather than as
		// an expansion — must still be data on the way out. (An *unquoted*
		// expansion is refused earlier; see TestWordsWithExpansionsAreNotTreatedAsVerbs.)
		{`app info '$(id)'`,
			[]string{"/system/bin/dumpsys", "package", "$(id)"}},
	}
	for _, c := range cases {
		r, handled, err := dispatch(t, c.in)
		if !handled || err != nil {
			t.Fatalf("%q: handled=%v err=%v", c.in, handled, err)
		}
		got := reparse(t, r.cmd)
		if len(got) != len(c.wantArgv) {
			t.Fatalf("%q built %q → argv %q, want %q", c.in, r.cmd, got, c.wantArgv)
		}
		for i := range got {
			if got[i] != c.wantArgv[i] {
				t.Fatalf("%q built %q → argv[%d] = %q, want %q", c.in, r.cmd, i, got[i], c.wantArgv[i])
			}
		}
	}
}

// TestWordsWithExpansionsAreNotTreatedAsVerbs: a command containing $(...) or
// a variable is not a verb invocation, and guessing at it would be worse than
// letting the shell handle it as an ordinary elevated command.
func TestWordsWithExpansionsAreNotTreatedAsVerbs(t *testing.T) {
	for _, cmd := range []string{
		"app uninstall $(cat /tmp/x)",
		"input text $HOME",
		"settings get global k && rm -rf /",
		"screenshot | tee /tmp/x",
	} {
		_, handled, _ := dispatch(t, cmd)
		if handled {
			t.Fatalf("%q was parsed as a verb; a word with an expansion must not be", cmd)
		}
	}
}

func TestAppSubcommands(t *testing.T) {
	cases := []struct{ in, want string }{
		{"app list", "/system/bin/pm list packages "},
		{"app list -3", "/system/bin/pm list packages '-3'"},
		{"app info com.example", "/system/bin/dumpsys package 'com.example'"},
		{"app uninstall com.example", "/system/bin/pm uninstall 'com.example'"},
		{"app stop com.example", "/system/bin/am force-stop 'com.example'"},
		{"app clear com.example", "/system/bin/pm clear 'com.example'"},
		{"app install /data/local/tmp/a.apk", "/system/bin/pm install -r '/data/local/tmp/a.apk'"},
	}
	for _, c := range cases {
		r, handled, err := dispatch(t, c.in)
		if !handled || err != nil {
			t.Fatalf("%q: handled=%v err=%v", c.in, handled, err)
		}
		if strings.TrimSpace(r.cmd) != strings.TrimSpace(c.want) {
			t.Fatalf("%q built %q, want %q", c.in, r.cmd, c.want)
		}
	}
	// app start uses the launcher intent: the activity to launch is not
	// derivable from the package name.
	r, _, err := dispatch(t, "app start com.example")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.cmd, "android.intent.category.LAUNCHER") {
		t.Fatalf("app start built %q, want it to go through the launcher intent", r.cmd)
	}

	for _, c := range []string{"app", "app info", "app info a b", "app frobnicate", "app uninstall"} {
		_, handled, err := dispatch(t, c)
		if !handled || err == nil {
			t.Fatalf("%q: handled=%v err=%v, want a usage error", c, handled, err)
		}
	}
}

func TestSettingsChecksTheNamespace(t *testing.T) {
	r, handled, err := dispatch(t, "settings get global adb_wifi_enabled")
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if r.cmd != "/system/bin/settings 'get' 'global' 'adb_wifi_enabled'" {
		t.Fatalf("cmd = %q", r.cmd)
	}

	// Android accepts exactly three namespaces; a typo otherwise produces an
	// unhelpful failure from the tool itself.
	_, handled, err = dispatch(t, "settings get globals x")
	if !handled || err == nil {
		t.Fatal("an unknown settings namespace was accepted")
	}
	if !strings.Contains(err.Error(), "system, secure or global") {
		t.Fatalf("error = %q, want it to list the valid namespaces", err)
	}

	for _, c := range []string{"settings", "settings get global", "settings put global k", "settings frob global k"} {
		_, handled, err := dispatch(t, c)
		if !handled || err == nil {
			t.Fatalf("%q: handled=%v err=%v, want a usage error", c, handled, err)
		}
	}
}

func TestProp(t *testing.T) {
	r, _, err := dispatch(t, "prop get ro.product.model")
	if err != nil {
		t.Fatal(err)
	}
	if r.cmd != "/system/bin/getprop 'ro.product.model'" {
		t.Fatalf("cmd = %q", r.cmd)
	}
	r, _, err = dispatch(t, "prop set service.adb.tcp.port 5555")
	if err != nil {
		t.Fatal(err)
	}
	if r.cmd != "/system/bin/setprop 'service.adb.tcp.port' '5555'" {
		t.Fatalf("cmd = %q", r.cmd)
	}
	if _, _, err := dispatch(t, "prop set only-a-name"); err == nil {
		t.Fatal("prop set accepted a missing value")
	}
}

// TestLogcatDefaultsToDumping: a following logcat over a request/response exec
// never returns and looks exactly like a hang.
func TestLogcatDefaultsToDumping(t *testing.T) {
	r, handled, err := dispatch(t, "logcat")
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if !strings.Contains(r.cmd, "-d") {
		t.Fatalf("cmd = %q, want a dump (-d) rather than a follow", r.cmd)
	}
	if _, _, err := dispatch(t, "logcat -f /tmp/x"); err == nil {
		t.Fatal("logcat -f accepted; on Android that writes a file on the device")
	}
}

func TestDispatchPassesThroughTheChannelAndExitCode(t *testing.T) {
	r := &recorder{code: 7}
	handled, via, code, err := Dispatch(context.Background(), "screenshot", elevate.KindSu, r, io.Discard)
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if code != 7 {
		t.Fatalf("code = %d, want the channel's own 7", code)
	}
	if via != elevate.KindSu {
		t.Fatalf("via = %q, want the channel that ran", via)
	}
	if r.via != elevate.KindSu {
		t.Fatalf("the pinned channel %q did not reach the elevator (got %q)", elevate.KindSu, r.via)
	}
}
