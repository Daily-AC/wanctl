package mcp

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// leanOutput is a command's output as the model receives it. Unless the
// operator set WANCTL_MCP_LEAN=1 that is exactly what the device sent; with it,
// the output is condensed first (see condenseOutput). The switch is read on
// every call, like WANCTL_MCP_ALLOW_UNSAFE_TRUST_SERVER.
//
// code is the command's exit status, and 0 for output of a command that is
// still running: there is no failure yet to read it for.
func leanOutput(s string, code int) string {
	if s == "" || os.Getenv("WANCTL_MCP_LEAN") != "1" {
		return s
	}
	return condenseOutput(s, code)
}

// foldAfter is the longest run kept as it is. A shorter one costs about what
// the line that would replace it costs.
const foldAfter = 3

// ansiSequence matches what a terminal acts on rather than shows: CSI (colour,
// cursor movement, erasing), OSC (titles, hyperlinks) ended by BEL or ST, the
// DCS/SOS/PM/APC strings, charset designations and the two-byte escapes. On the
// way to the model each one becomes visible text such as \x1b[1;32m — tokens
// that say nothing about what the command did.
var ansiSequence = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[PX^_][^\x1b]*\x1b\\|[()][0-9A-Za-z]|[78=>DEHMc])`)

// passedTest matches a line, trimmed, that only reports a test that passed, as
// go test (-v), cargo test, pytest -v, jest, vitest, mocha, bun and node --test
// print it. Failures, skips, logs and summaries never match: they are what a
// failed run is read for.
var passedTest = regexp.MustCompile(`^(?:` +
	`=== (?:RUN|PAUSE|CONT)\s+\S.*` +
	`|--- PASS:\s+\S.*` +
	`|PASS(?:\s+\S.*)?` +
	`|ok\s+\S+\s+(?:\(cached\)|[0-9.]+s)(?:\s.*)?` +
	`|\?\s+\S+\s+\[no test files\]` +
	`|test .+ \.\.\. ok` +
	`|\S+::\S+\s+PASSED(?:\s+\[\s*\d+%\])?` +
	`|\(pass\)\s+\S.*` +
	`|[✓✔√]\s+\S.*` +
	`)$`)

// condenseOutput takes out of command output what a terminal would have hidden
// and what a person would skim past:
//
//   - escape sequences;
//   - the frames of a progress bar or spinner, redrawn with a carriage return:
//     only the last thing written on a line is kept;
//   - more than foldAfter identical lines in a row: the first stays, followed
//     by a line saying how many more there were;
//   - when the command failed (code != 0), more than foldAfter lines in a row
//     that only report passing tests, replaced by a line counting them. A run
//     that passed is left whole: what ran is then the answer.
//
// Every fold says what it took. Nothing changes on the device: offsets, byte
// counts and the spilled copy still describe what the command wrote, and
// wanctl_read is never condensed, so the exact bytes are one redirect away.
func condenseOutput(s string, code int) string {
	lines := strings.Split(resolveCarriageReturns(ansiSequence.ReplaceAllString(s, "")), "\n")
	if code != 0 {
		lines = foldPassedTests(lines)
	}
	return strings.Join(collapseRepeats(lines), "\n")
}

// resolveCarriageReturns keeps, of each line, what was written after its last
// carriage return — what the terminal ended up showing. A line that ends with
// one, or is cleared with spaces before the next frame, keeps the last
// non-blank text instead, so the final state of a progress bar survives. CRLF
// is a line ending, not a redraw.
func resolveCarriageReturns(s string) string {
	if !strings.Contains(s, "\r") {
		return s
	}
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i, line := range lines {
		if !strings.Contains(line, "\r") {
			continue
		}
		frames := strings.Split(line, "\r")
		lines[i] = ""
		for j := len(frames) - 1; j >= 0; j-- {
			if strings.TrimSpace(frames[j]) != "" {
				lines[i] = frames[j]
				break
			}
		}
	}
	return strings.Join(lines, "\n")
}

// foldPassedTests replaces each run of more than foldAfter passing-test lines
// with a line counting them.
func foldPassedTests(lines []string) []string {
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); {
		j := i
		for j < len(lines) && passedTest.MatchString(strings.TrimSpace(lines[j])) {
			j++
		}
		if j == i {
			out = append(out, lines[i])
			i++
			continue
		}
		run, announced := lines[i:j], ""
		if n := len(run); j < len(lines) && strings.HasPrefix(strings.TrimSpace(run[n-1]), "=== ") {
			// go test -v names a test before its output: the output that
			// follows the run belongs to the last test announced, often the
			// one that failed, so its name stays next to it.
			run, announced = run[:n-1], run[n-1]
		}
		if len(run) > foldAfter {
			out = append(out, fmt.Sprintf("[... %d passing-test lines omitted ...]", len(run)))
		} else {
			out = append(out, run...)
		}
		if announced != "" {
			out = append(out, announced)
		}
		i = j
	}
	return out
}

// collapseRepeats keeps the first of more than foldAfter identical non-blank
// lines in a row and says how many more there were.
func collapseRepeats(lines []string) []string {
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); {
		j := i + 1
		for j < len(lines) && lines[j] == lines[i] {
			j++
		}
		if n := j - i; n > foldAfter && strings.TrimSpace(lines[i]) != "" {
			out = append(out, lines[i], fmt.Sprintf("[... previous line repeated %d more times ...]", n-1))
		} else {
			out = append(out, lines[i:j]...)
		}
		i = j
	}
	return out
}
