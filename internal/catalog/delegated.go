package catalog

import (
	"fmt"
	"strings"
)

// A delegated session is wanctl reached through a short-lived grant rather than
// through a login: WebFetch today, any later adapter that hands a chat model a
// URL instead of a connector. Such a client holds four primitives, not
// twenty-one — no background jobs, no byte transfer, no screenshot — and its
// exec is one-shot, because the device refuses a persistent shell and
// exec_async/exec_poll on a delegated session (docs/adr/0009-webfetch-long-jobs.md).
//
// The instructions it reads are therefore not the harness prompt with lines
// deleted, and they are not a second copy of it either. What still holds is
// rendered from the same catalog Instructions() renders from; what does not is
// declared here, next to the exact text it replaces, and a test holds each of
// those quotes to the catalog word for word. Rewriting a rule upstream then
// fails CI on this file until someone decides what the delegated client should
// be told — which is the thing a copy silently skips.

// instructionsHeader is the claim both instruction surfaces open with. It is
// one string rather than two writes so a reworded promise cannot reach an MCP
// host and a delegated client in two different forms.
const instructionsHeader = "wanctl is " + Headline + ": your hands and eyes on a machine\n" +
	"you do not run on, behind that device owner's policy. A refusal is an answer.\n\n"

// delegatedTools are those four primitives, in the order a caller meets them,
// under the names the delegated protocol spells them with. Instead names the
// catalog's own index line when it cannot be used as it stands.
var delegatedTools = []struct{ Name, MCPName, Line, Instead string }{
	{Name: "exec", MCPName: "wanctl_exec",
		// The catalog line advertises the persistent shell an MCP host gets.
		// A delegated exec has no session: every call starts in its own shell.
		Line:    "run a command on a device (one-shot, no session state)",
		Instead: "run a command or script on a device (persistent shell)"},
	{Name: "read_text", MCPName: "wanctl_read"},
	{Name: "edit_text", MCPName: "wanctl_edit"},
	{Name: "write_text", MCPName: "wanctl_write"},
}

// delegatedRules is the DEV LOOP as a delegated client can follow it. Source is
// the catalog rule, quoted; Rule is what this client is told instead, or "" for
// a rule that is about primitives it does not have.
var delegatedRules = []struct{ Source, Rule string }{
	{
		Source: "wanctl_exec keeps a persistent shell per device: cd once and stay there. Long work goes to wanctl_exec_async, then wanctl_exec_poll until it is done.",
		// Neither half survives delegation: no shell persists between calls, and
		// long work stays in one exec with a longer timeout because the device
		// refuses background jobs here. Pass cwd instead of cd; the protocol's
		// own limits document the timeout.
		Rule: "exec is one-shot: pass cwd rather than cd, and give long work a longer timeout_seconds instead of a background job.",
	},
	{
		Source: "Read with wanctl_read, patch with wanctl_edit (several {old,new} in ONE call), write files with wanctl_write. Never cat/sed/echo a file through a shell.",
		// Same rule minus the batch form, which the URL-shaped call cannot take:
		// one old/new per edit_text.
		Rule: "Read with read_text, patch with edit_text (one span per call), write files with write_text. Never cat/sed/echo a file through a shell.",
	},
	{
		Source: "Over-long exec output returns its TAIL; the rest waits in a device file.",
		// The opposite is true here: a delegated exec keeps the FIRST 16 KiB of
		// each stream and cancels the command, so the end a tail rule promises
		// is exactly what is missing. Bound the output before running it.
		Rule: "Over-long exec output is cut off and the command is cancelled, so filter or tail on the device rather than printing a whole file.",
	},
	{
		Source: "Before working in a project directory, read its AGENTS.md or CLAUDE.md with wanctl_read if one exists and follow it: it outranks how you would proceed.",
		Rule:   "Before working in a project directory, read its AGENTS.md or CLAUDE.md with read_text if one exists and follow it: it outranks how you would proceed.",
	},
}

// DelegatedInstructions is the harness prompt for a delegated client: the same
// claim, the four primitives it actually has, and the dev loop as it applies to
// them. It carries no REFUSALS block — a delegated client never sees those
// texts, because the adapter answers it with error_code values and its own
// security rules instead — and no login, transfer or screenshot primitive.
func DelegatedInstructions() string {
	var b strings.Builder
	b.WriteString(instructionsHeader)
	for _, t := range delegatedTools {
		b.WriteString(fmt.Sprintf("  %-20s %s\n", t.Name, delegatedLine(t.MCPName, t.Line)))
	}
	b.WriteString("\nDEV LOOP\n")
	for _, r := range delegatedRules {
		if r.Rule == "" {
			continue
		}
		b.WriteString(wrapPlainPrefixed(r.Rule, Width-2, "  ") + "\n")
	}
	return b.String()
}

// delegatedLine prefers the catalog's own index line, so three of the four
// primitives are described by the one place that describes them everywhere.
func delegatedLine(mcpName, override string) string {
	if override != "" {
		return override
	}
	c, _ := Lookup(mcpName)
	return instructionLine(c)
}

// DelegatedTools are the primitive names a delegated client is given, for a
// caller that has to check its own surface against this list.
func DelegatedTools() []string {
	out := make([]string, 0, len(delegatedTools))
	for _, t := range delegatedTools {
		out = append(out, t.Name)
	}
	return out
}
