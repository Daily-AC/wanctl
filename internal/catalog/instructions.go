package catalog

import (
	"fmt"
	"strings"
)

// Instructions is the harness's own system prompt: what wanctl is, the whole
// list of primitives, how they fit into a loop, and the four refusals a caller
// has to recognise.
//
// MCP has a place for exactly this — the `instructions` field of the initialize
// response — and until now wanctl left it empty, so a host learned the tools
// one description at a time, in whatever order it happened to read them, with
// nothing anywhere saying how they compose. A tool description can say what
// `edit` does; only this can say to read the project's AGENTS.md first.
//
// It is assembled from the same catalog the tools are registered from, so a
// primitive cannot appear here and not exist, and the error texts quoted below
// are the ones the tools actually return. `wanctl help --instructions` prints
// it, which is how anything outside the MCP server — the discovery page, a
// person deciding whether to trust this thing — reads the same words.
//
// The budget is forty lines. It is read before any work, every session, by
// something that pays for every token; the command reference is one
// `wanctl help <command>` away and does not belong here.
func Instructions() string {
	var b strings.Builder
	b.WriteString("wanctl is " + Headline + ": your hands and eyes on a machine\n")
	b.WriteString("you do not run on, behind that device owner's policy. A refusal is an answer.\n\n")

	for _, c := range MCPCommands() {
		b.WriteString(fmt.Sprintf("  %-20s %s\n", c.MCPName, instructionLine(c)))
	}

	b.WriteString("\nDEV LOOP\n")
	for _, rule := range devLoop {
		b.WriteString(wrapPlainPrefixed(rule, Width-2, "  ") + "\n")
	}

	b.WriteString("\nREFUSALS — none of these mean try again:\n")
	for _, r := range criticalErrors {
		b.WriteString("  " + r.Text + ": " + r.Do + "\n")
	}
	return b.String()
}

// wrapPlainPrefixed lays one rule out inside the eighty columns the rest of the
// contract lives in, so the block stays readable in a terminal and in whatever
// a host renders an instructions field as.
func wrapPlainPrefixed(s string, width int, prefix string) string {
	lines := strings.Split(wrapPlain(s, width), "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

// devLoop is the part no single tool description can carry: which primitive to
// reach for, in what order, and what not to do instead. Every line here was a
// mistake an agent made in the field before it was written down.
var devLoop = []string{
	"wanctl_exec keeps a persistent shell per device: cd once and stay there. Long work goes to wanctl_exec_async, then wanctl_exec_poll until it is done.",
	"Read with wanctl_read, patch with wanctl_edit (several {old,new} in ONE call), write files with wanctl_write. Never cat/sed/echo a file through a shell.",
	"Over-long exec output returns its TAIL; the rest waits in a device file.",
	"Before working in a project directory, read its AGENTS.md or CLAUDE.md with wanctl_read if one exists and follow it: it outranks how you would proceed.",
}

// criticalErrors are the four refusals that mean the next move is not "try
// again", each with the shortest form of what to do instead.
//
// The texts are quoted verbatim because agents match on them, and a test holds
// every one of them to a failure the catalog actually declares — so a refusal
// renamed in a tool description cannot go on being advertised here. The actions
// are deliberately shorter than the catalog's own: this list is read before any
// work, and the full explanation is one `wanctl help <command>` away.
var criticalErrors = []struct{ Text, Do string }{
	{"PAIRING REQUIRED", "give the URL in the message to the user, then retry."},
	{"DEVICE IDENTITY CONFIRMATION REQUIRED", "call wanctl_trust_server, retry."},
	{"DEVICE IDENTITY MISMATCH", "refused, nothing sent; report both fingerprints."},
	{"LOGIN REQUIRED", "call wanctl_login; a saved rebind restores it instantly."},
}

// CriticalErrors are the error texts the instructions promise a caller will
// recognise. Exported for the test that checks each is a failure some command
// really declares.
func CriticalErrors() []string {
	out := make([]string, 0, len(criticalErrors))
	for _, e := range criticalErrors {
		out = append(out, e.Text)
	}
	return out
}

// Declares reports whether any command declares this exact failure text.
func Declares(text string) bool {
	for _, c := range Commands {
		for _, f := range c.Errors {
			if f.Text == text {
				return true
			}
		}
	}
	return false
}

// instructionLine is the shortest honest description of a primitive: the index
// line where there is one, the summary otherwise, lowercased so the list reads
// as a list.
func instructionLine(c Command) string {
	line := c.IndexLine
	if line == "" {
		line = c.Summary
	}
	if line == "" {
		return ""
	}
	return strings.ToLower(line[:1]) + line[1:]
}
