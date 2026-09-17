package catalog

import (
	"fmt"
	"sort"
	"strings"
)

// Width is the column the renderers wrap at. Eighty is the width of the
// narrowest terminal anyone still uses, and the index is asserted against it.
const Width = 80

// Index is the short listing bare `wanctl` prints: one line per command, no
// prose. It is deliberately not the whole contract — the whole contract is
// `wanctl help <command>` and `wanctl help --markdown`, and a wall of text at
// the entry point is a wall nobody reads.
func Index(relay, portal string) string {
	var b strings.Builder
	b.WriteString("wanctl — " + Headline + ", over an encrypted relay\n")
	b.WriteString("USAGE: wanctl <command> [flags]   ·   wanctl help <command>  explains one\n")
	for _, g := range IndexGroups {
		head := " " + string(g.Group)
		if g.Hint != "" {
			head += " " + g.Hint
		}
		b.WriteString(head + "\n")
		for _, c := range groupMembers(g.Group) {
			b.WriteString(fmt.Sprintf("  %-11s %s\n", c.Name, c.IndexLine))
		}
	}
	for i, line := range strings.Split(wrapPlain(strings.Join(moreCommands(), " "), Width-7), "\n") {
		lead := " MORE  "
		if i > 0 {
			lead = "       "
		}
		b.WriteString(lead + line + "\n")
	}
	for _, line := range defaultsLines(relay, portal) {
		b.WriteString(line + "\n")
	}
	return b.String()
}

// groupMembers are the index rows of one group, in IndexRank order.
func groupMembers(g Group) []Command {
	var out []Command
	for _, c := range Commands {
		if c.Group == g && c.Name != "" && c.IndexLine != "" {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].IndexRank < out[j].IndexRank })
	return out
}

// moreCommands are the entries the index names but does not describe: servers,
// instance administration and the settings verbs. They each still have a full
// help entry; what they do not have is a claim on the twenty lines a newcomer
// actually reads.
func moreCommands() []string {
	var out []string
	for _, c := range Commands {
		if c.Name == "" || c.Group != "" || strings.Contains(c.Name, " ") || c.Name == "help" {
			continue
		}
		out = append(out, c.Name)
	}
	return out
}

// defaultsLines prints where this binary connects, wrapping rather than
// overflowing when a self-hosted deployment has long URLs.
func defaultsLines(relay, portal string) []string {
	if relay == "" {
		relay = "(not set)"
	}
	if portal == "" {
		portal = "(not set)"
	}
	one := fmt.Sprintf("Defaults: relay=%s portal=%s  (change with 'wanctl config set')", relay, portal)
	if len([]rune(one)) <= Width {
		return []string{one}
	}
	split := []string{
		fmt.Sprintf("Defaults: relay=%s", relay),
		fmt.Sprintf("          portal=%s  ('wanctl config set' to change)", portal),
	}
	for _, line := range split {
		if len([]rune(line)) > Width {
			// A URL long enough to overflow is not worth truncating into
			// something that reads like an address but is not one.
			return []string{"Defaults: run 'wanctl config' to see the relay and portal in use"}
		}
	}
	return split
}

// Entry renders one command for a terminal: what it is, the description both
// surfaces share, its parameters, an example per surface, and the error texts
// a caller has to branch on.
func Entry(c Command) string {
	var b strings.Builder
	title := c.Name
	if title == "" {
		title = c.MCPName
	} else {
		title = "wanctl " + title
	}
	b.WriteString(title + " — " + c.Summary + "\n")
	switch {
	case c.Name == "":
		b.WriteString("MCP tool only: " + c.MCPName + " (no CLI command)\n")
	case c.MCPName != "":
		b.WriteString("MCP tool: " + c.MCPName + "\n")
	default:
		b.WriteString("CLI only (no MCP tool)\n")
	}
	b.WriteString("\n" + wrap(c.CLIDescription(), Width-2, "  ") + "\n")

	if params := c.CLIParams(); len(params) > 0 {
		b.WriteString("\nPARAMETERS\n")
		for _, p := range params {
			b.WriteString(fmt.Sprintf("  %-28s %s\n", p.CLISpelling(), kind(p)))
			b.WriteString(wrap(p.CLIMeaning(), Width-6, "      ") + "\n")
		}
	}
	if c.CLIExample != "" || c.MCPExample != "" {
		b.WriteString("\nEXAMPLE\n")
		if c.CLIExample != "" {
			for _, line := range strings.Split(c.CLIExample, "\n") {
				b.WriteString("  " + strings.TrimLeft(line, " ") + "\n")
			}
		}
		if c.MCPExample != "" {
			b.WriteString("  MCP: " + c.MCPExample + "\n")
		}
	}
	if len(c.Errors) > 0 {
		b.WriteString("\nERRORS the caller must react to\n")
		for _, e := range c.Errors {
			b.WriteString("  " + e.Text + "\n")
			b.WriteString(wrap(e.Means, Width-6, "      ") + "\n")
		}
	}
	return b.String()
}

// kind is the parameter's type line: what the MCP schema calls it, and whether
// the tool refuses without it.
func kind(p Param) string {
	s := typeLabel(p)
	if p.Required {
		s += ", required"
	}
	if p.CLIOnly {
		s += ", CLI only"
	}
	return s
}

// typeLabel names a parameter's type the way a reader needs it. For an array
// that means saying what one element looks like, read from the item schema the
// tool actually registers rather than from a sentence kept in step by hand:
// "array of {old, new}" is only true while `edits` is the only array there is.
func typeLabel(p Param) string {
	if p.Type != TypeArray {
		return p.Type
	}
	return "array of " + itemLabel(p.Items)
}

// itemLabel describes one element of an array parameter.
func itemLabel(items map[string]any) string {
	if items == nil {
		return "values"
	}
	typ, _ := items["type"].(string)
	props, _ := items["properties"].(map[string]any)
	if typ != "object" || len(props) == 0 {
		if typ == "" {
			return "values"
		}
		return typ + " values"
	}
	return "{" + strings.Join(fieldOrder(items, props), ", ") + "}"
}

// fieldOrder lists an object's fields in the order the schema declares them
// required, which is the order a caller writes them, falling back to a sorted
// listing so the output is at least stable.
func fieldOrder(items map[string]any, props map[string]any) []string {
	var named []string
	switch req := items["required"].(type) {
	case []string:
		named = append(named, req...)
	case []any:
		for _, r := range req {
			if name, ok := r.(string); ok {
				named = append(named, name)
			}
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, name := range named {
		if _, ok := props[name]; ok && !seen[name] {
			seen[name], out = true, append(out, name)
		}
	}
	rest := make([]string, 0, len(props))
	for name := range props {
		if !seen[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// Markdown renders the whole catalog. docs/contract.md is this output, and a
// test fails when the file and this function disagree.
func Markdown() string {
	var b strings.Builder
	b.WriteString("# wanctl command contract\n\n")
	b.WriteString(mdParagraphs(Product) + "\n\n")
	b.WriteString("This file is generated. It is the output of `wanctl help --markdown`, and\n")
	b.WriteString("the same catalog (`internal/catalog`) produces the CLI help and the MCP tool\n")
	b.WriteString("descriptions, so the three cannot drift. Regenerate with:\n\n")
	b.WriteString("```\ngo run . help --markdown > docs/contract.md\n```\n\n")

	b.WriteString("## Instructions\n\n")
	b.WriteString("This is what an MCP host is handed before it calls anything — the\n")
	b.WriteString("`instructions` field of the initialize response, and the output of\n")
	b.WriteString("`wanctl help --instructions`. It is the harness's system prompt.\n\n")
	b.WriteString("```\n" + Instructions() + "```\n\n")

	b.WriteString("## Commands\n\n")
	b.WriteString("| Command | MCP tool | Summary |\n|---|---|---|\n")
	for _, c := range Commands {
		b.WriteString("| " + mdCell(c.Name) + " | " + mdCell(c.MCPName) + " | " + escapePipes(c.Summary) + " |\n")
	}
	b.WriteString("\n")

	for _, c := range Commands {
		b.WriteString("## " + headingFor(c) + "\n\n")
		b.WriteString("*" + escapePipes(c.Summary) + "*\n\n")
		b.WriteString(mdParagraphs(c.Desc) + "\n")
		if c.CLINote != "" {
			b.WriteString("\n**On the command line.**\n\n" + mdParagraphs(c.CLINote) + "\n")
		}
		if c.MCPNote != "" {
			b.WriteString("\n**As an MCP tool.**\n\n" + mdParagraphs(c.MCPNote) + "\n")
		}
		if len(c.Params) > 0 {
			b.WriteString("\n| Parameter | CLI | Type | Required | Meaning |\n|---|---|---|---|---|\n")
			for _, p := range c.Params {
				cli := "`" + escapePipes(p.CLISpelling()) + "`"
				if p.CLI == NotOnCLI || p.MCPOnly {
					cli = "—"
				}
				mcpName := "`" + p.Name + "`"
				if p.CLIOnly {
					mcpName = "—"
				}
				req := "no"
				if p.Required {
					req = "**yes**"
				}
				typ := typeLabel(p)
				meaning := escapePipes(p.Desc)
				if p.CLIDesc != "" {
					// The two surfaces genuinely disagree about this argument,
					// so the contract has to print both rather than pick one.
					meaning += " **On the CLI:** " + escapePipes(p.CLIDesc)
				}
				b.WriteString("| " + mcpName + " | " + cli + " | " + typ + " | " + req + " | " + meaning + " |\n")
			}
		}
		if c.CLIExample != "" {
			b.WriteString("\n```\n" + c.CLIExample + "\n```\n")
		}
		if c.MCPExample != "" {
			b.WriteString("\n```\n" + c.MCPExample + "\n```\n")
		}
		if len(c.Errors) > 0 {
			b.WriteString("\n| Error | What to do |\n|---|---|\n")
			for _, e := range c.Errors {
				b.WriteString("| `" + escapePipes(e.Text) + "` | " + escapePipes(e.Means) + " |\n")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

func headingFor(c Command) string {
	switch {
	case c.Name != "" && c.MCPName != "":
		return "`wanctl " + c.Name + "` / `" + c.MCPName + "`"
	case c.Name != "":
		return "`wanctl " + c.Name + "`"
	default:
		return "`" + c.MCPName + "`"
	}
}

func mdCell(s string) string {
	if s == "" {
		return "—"
	}
	return "`" + s + "`"
}

func escapePipes(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

func mdParagraphs(s string) string {
	paras := strings.Split(s, "\n\n")
	for i, p := range paras {
		paras[i] = wrapPlain(strings.ReplaceAll(p, "\n", " "), 78)
	}
	return strings.Join(paras, "\n\n")
}

// wrap breaks text to width, prefixing every line, and keeps blank-line
// paragraph breaks from the source.
func wrap(s string, width int, prefix string) string {
	var out []string
	for _, para := range strings.Split(s, "\n\n") {
		if len(out) > 0 {
			out = append(out, strings.TrimRight(prefix, " "))
		}
		for _, line := range strings.Split(wrapPlain(strings.ReplaceAll(para, "\n", " "), width), "\n") {
			out = append(out, prefix+line)
		}
	}
	return strings.Join(out, "\n")
}

// wrapPlain is a greedy word wrap that counts runes, not bytes: these
// descriptions contain — and ✓ and CJK punctuation, and a byte count would
// break lines early and unevenly.
func wrapPlain(s string, width int) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var lines []string
	line := words[0]
	for _, w := range words[1:] {
		if len([]rune(line))+1+len([]rune(w)) > width {
			lines = append(lines, line)
			line = w
			continue
		}
		line += " " + w
	}
	return strings.Join(append(lines, line), "\n")
}

// Names lists every name either surface accepts, for an error message that has
// to show what does exist.
func Names() []string {
	var out []string
	for _, c := range Commands {
		if c.Name != "" {
			out = append(out, c.Name)
		}
	}
	sort.Strings(out)
	return out
}
