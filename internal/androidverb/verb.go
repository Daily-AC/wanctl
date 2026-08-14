// Package androidverb turns a handful of high-frequency Android operations
// into commands worth typing, on top of the elevation channel that makes them
// possible at all (internal/elevate).
//
// The elevation channel already gives a controller the whole adb surface —
// `wanctl exec --elevate -- pm list packages` works with nothing in this
// package. What it does not give is a good shape for the parts of that surface
// where the raw tool is awkward over a pipe: `screencap` writes a PNG to
// stdout, `input text` needs its argument escaped in a way that is easy to get
// wrong, and "list the installed apps" is three different tools depending on
// what you want to know.
//
// So these verbs are a thin translation layer and nothing more. Each one turns
// into exactly one shell command, run through the same channel, gated by the
// same elevated policy class, and recorded in the same audit log. Nothing here
// can do anything a controller could not do by hand; it exists so that the
// common cases do not require remembering which of `pm`, `am`, `cmd` and
// `dumpsys` owns the operation this week.
package androidverb

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"wanctl/internal/elevate"
)

// Elevator is the part of elevate.Manager these verbs need.
type Elevator interface {
	Run(ctx context.Context, via elevate.Kind, command, cwd string, out io.Writer) (elevate.Kind, int, error)
}

// Dispatch runs command as a structured verb.
//
// handled is false when command does not name a verb, which is the signal to
// the caller that it is an ordinary pass-through command. That check is on the
// first word only: a verb name is never a prefix that could swallow something
// else, and an unknown first word is not this package's business.
func Dispatch(ctx context.Context, command string, via elevate.Kind, el Elevator, out io.Writer) (handled bool, ranVia elevate.Kind, code int, err error) {
	argv, ok := splitArgs(command)
	if !ok || len(argv) == 0 {
		return false, "", 0, nil
	}
	build, ok := verbs[argv[0]]
	if !ok {
		return false, "", 0, nil
	}
	shellCmd, err := build(argv[1:])
	if err != nil {
		// A usage error is the verb's own answer, not a failure of the device.
		return true, "", 0, err
	}
	ranVia, code, err = el.Run(ctx, via, shellCmd, "", out)
	return true, ranVia, code, err
}

// Names lists the verbs, for help text and for tests that assert the set.
func Names() []string {
	out := make([]string, 0, len(verbs))
	for name := range verbs {
		out = append(out, name)
	}
	return out
}

// builder turns a verb's arguments into one shell command.
type builder func(args []string) (string, error)

var verbs = map[string]builder{
	"screenshot": screenshot,
	"input":      input,
	"app":        app,
	"settings":   settings,
	"prop":       prop,
	"logcat":     logcat,
}

// splitArgs parses the command the way a shell would, so a quoted argument
// survives as one word.
//
// The controller joins argv with spaces before it reaches the wire, and a local
// shell has usually eaten the quotes before that (the hazard v0.1.12 documents
// and warns about). Parsing here recovers the case where the quotes did survive
// — an MCP caller, or `exec -script` — and produces a clean refusal rather than
// a mangled command for input that is not a single simple command at all.
func splitArgs(command string) ([]string, bool) {
	f, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil || len(f.Stmts) != 1 {
		return nil, false
	}
	call, ok := f.Stmts[0].Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) == 0 {
		return nil, false
	}
	var argv []string
	printer := syntax.NewPrinter()
	for _, w := range call.Args {
		lit, ok := literal(w)
		if !ok {
			// A word with an expansion in it ($x, `cmd`, a glob). Not something
			// a verb argument should contain, and not something to guess at.
			var sb strings.Builder
			printer.Print(&sb, w)
			return nil, false
		}
		argv = append(argv, lit)
	}
	return argv, true
}

// literal renders a word if it is entirely literal (possibly quoted), and
// reports false if any part of it would be expanded by a shell.
func literal(w *syntax.Word) (string, bool) {
	var sb strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			sb.WriteString(p.Value)
		case *syntax.SglQuoted:
			sb.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok {
					return "", false
				}
				sb.WriteString(lit.Value)
			}
		default:
			return "", false
		}
	}
	return sb.String(), true
}

// quote renders s as a single POSIX shell word. Every verb argument goes
// through this: the values are user data (a package name, a text string to
// type, a settings value) and they are about to be concatenated into shell
// source that will run as root.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func quoteAll(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = quote(a)
	}
	return strings.Join(out, " ")
}

// usage is a verb's refusal, phrased as what to type instead.
func usage(format string, a ...any) error {
	return fmt.Errorf(format, a...)
}

// screenshot writes a PNG to stdout.
//
// `screencap -p` is the whole implementation. The reason this is a verb rather
// than advice in the docs is the failure it prevents: run it without redirecting
// and a megabyte of PNG goes to the terminal. The controller's `wanctl
// screenshot` wrapper redirects for you; this refuses arguments it does not
// understand so a typo cannot silently produce a broken file.
func screenshot(args []string) (string, error) {
	if len(args) != 0 {
		return "", usage("screenshot takes no arguments on the device " +
			"(use `wanctl screenshot <device> -o file.png`, or redirect stdout yourself)")
	}
	return "/system/bin/screencap -p", nil
}

// input drives the touchscreen and keyboard.
func input(args []string) (string, error) {
	if len(args) == 0 {
		return "", usage("input needs a subcommand: tap X Y | swipe X1 Y1 X2 Y2 [DURATION_MS] | text STRING | key KEYCODE")
	}
	switch args[0] {
	case "tap":
		if len(args) != 3 || !allInts(args[1:]) {
			return "", usage("input tap needs two integer coordinates: input tap X Y")
		}
	case "swipe":
		if n := len(args); (n != 5 && n != 6) || !allInts(args[1:]) {
			return "", usage("input swipe needs four integer coordinates and an optional duration: input swipe X1 Y1 X2 Y2 [DURATION_MS]")
		}
	case "text":
		if len(args) != 2 {
			return "", usage("input text needs exactly one string: input text 'hello world' " +
				"(quote it, or your local shell splits it into separate words)")
		}
	case "key":
		if len(args) != 2 {
			return "", usage("input key needs one keycode: input key KEYCODE_HOME (or its number)")
		}
	default:
		return "", usage("unknown input subcommand %q: want tap, swipe, text or key", args[0])
	}
	return "/system/bin/input " + quoteAll(args), nil
}

// app is the one noun over the several tools that own application state.
func app(args []string) (string, error) {
	if len(args) == 0 {
		return "", usage("app needs a subcommand: list [-3|-s] | info PKG | install APK | uninstall PKG | start PKG | stop PKG | clear PKG")
	}
	sub, rest := args[0], args[1:]
	needPkg := func() (string, error) {
		if len(rest) != 1 {
			return "", usage("app %s needs exactly one package name", sub)
		}
		return rest[0], nil
	}
	switch sub {
	case "list":
		// -3 third-party only, -s system only; anything else is a filter word
		// pm already understands, so pass it through quoted.
		return "/system/bin/pm list packages " + quoteAll(rest), nil
	case "info":
		pkg, err := needPkg()
		if err != nil {
			return "", err
		}
		return "/system/bin/dumpsys package " + quote(pkg), nil
	case "install":
		if len(rest) != 1 {
			return "", usage("app install needs one APK path ON THE DEVICE " +
				"(push it there first: wanctl push <device> local.apk /data/local/tmp/a.apk)")
		}
		return "/system/bin/pm install -r " + quote(rest[0]), nil
	case "uninstall":
		pkg, err := needPkg()
		if err != nil {
			return "", err
		}
		return "/system/bin/pm uninstall " + quote(pkg), nil
	case "start":
		pkg, err := needPkg()
		if err != nil {
			return "", err
		}
		// The launcher intent rather than a guessed activity name: the
		// component a package launches with is not derivable from its name.
		return "/system/bin/monkey -p " + quote(pkg) + " -c android.intent.category.LAUNCHER 1", nil
	case "stop":
		pkg, err := needPkg()
		if err != nil {
			return "", err
		}
		return "/system/bin/am force-stop " + quote(pkg), nil
	case "clear":
		pkg, err := needPkg()
		if err != nil {
			return "", err
		}
		return "/system/bin/pm clear " + quote(pkg), nil
	default:
		return "", usage("unknown app subcommand %q: want list, info, install, uninstall, start, stop or clear", sub)
	}
}

// settingsNamespaces is the closed set Android accepts. Checking it here turns
// the platform's silent-ish failure into a message that names the mistake.
var settingsNamespaces = map[string]bool{"system": true, "secure": true, "global": true}

func settings(args []string) (string, error) {
	if len(args) < 3 {
		return "", usage("settings needs: settings get NAMESPACE KEY | settings put NAMESPACE KEY VALUE")
	}
	op, ns := args[0], args[1]
	if !settingsNamespaces[ns] {
		return "", usage("unknown settings namespace %q: want system, secure or global", ns)
	}
	switch op {
	case "get":
		if len(args) != 3 {
			return "", usage("settings get needs: settings get %s KEY", ns)
		}
	case "put":
		if len(args) != 4 {
			return "", usage("settings put needs: settings put %s KEY VALUE", ns)
		}
	default:
		return "", usage("unknown settings operation %q: want get or put", op)
	}
	return "/system/bin/settings " + quoteAll(args), nil
}

func prop(args []string) (string, error) {
	if len(args) == 0 {
		return "", usage("prop needs: prop get [NAME] | prop set NAME VALUE")
	}
	switch args[0] {
	case "get":
		if len(args) > 2 {
			return "", usage("prop get takes at most one property name")
		}
		return "/system/bin/getprop " + quoteAll(args[1:]), nil
	case "set":
		if len(args) != 3 {
			return "", usage("prop set needs: prop set NAME VALUE")
		}
		return "/system/bin/setprop " + quoteAll(args[1:]), nil
	default:
		return "", usage("unknown prop operation %q: want get or set", args[0])
	}
}

// logcat defaults to dumping and exiting rather than following.
//
// A following logcat over a request/response exec never returns, and the
// controller has no way to say "stop" on that path — it would look like a hang.
// `-d` is therefore the default, and anyone who wants a live tail can pass
// their own flags and use `wanctl exec_async` to hold it.
func logcat(args []string) (string, error) {
	for _, a := range args {
		if a == "-f" {
			return "", usage("logcat -f writes to a file on the device, which is probably not what you meant; " +
				"to follow the log use `wanctl exec_async --elevate -- logcat` and poll it")
		}
	}
	if len(args) == 0 {
		return "/system/bin/logcat -d -v time -t 200", nil
	}
	return "/system/bin/logcat " + quoteAll(args), nil
}

func allInts(args []string) bool {
	for _, a := range args {
		if _, err := strconv.Atoi(a); err != nil {
			return false
		}
	}
	return true
}
