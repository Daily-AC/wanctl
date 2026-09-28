package main

import (
	"io"
	"os"

	"golang.org/x/term"

	"wanctl/internal/termsafe"
)

// isTerminal is a variable so tests can decide the answer.
var isTerminal = func(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// deviceOutput is where to write what a device sent back: command output, file
// contents, anything else a device or its owner chose.
//
// A pipe or a file gets the bytes exactly as they came. That is a script's
// output, a pulled file, a PNG, and changing one byte of it would be a bug. A
// terminal gets them as text, with the control characters that would otherwise
// drive the terminal made visible (internal/termsafe), because a device
// controlled by someone else must not be able to rewrite what this terminal
// shows. Call flush before exiting; it writes the tail of a character a device
// left unfinished.
func deviceOutput(f *os.File) (w io.Writer, flush func()) {
	if !isTerminal(f) {
		return f, func() {}
	}
	t := termsafe.NewWriter(f)
	return t, func() { t.Flush() }
}

// errorText is how an error is printed: always escaped, terminal or not. An
// error can carry a device's own words ("remote error: …") or a pairing link
// the device chose, and a log file of stderr is read on a terminal later.
func errorText(err error) string { return termsafe.Escape(err.Error()) }
