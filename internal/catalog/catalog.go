// Package catalog is the single written contract for wanctl's primitives.
//
// One value per command describes what it does, what each parameter means and
// which error texts a caller has to react to. Three readers consume it and none
// of them keeps a second copy: the MCP server builds its tool descriptions from
// it (internal/mcp), the CLI renders `wanctl help` from it (main), and
// docs/contract.md is its Markdown rendering, checked for drift by a test.
//
// The descriptions are not reference prose. wanctl is the external harness for
// a web AI, so what an MCP host loads from this catalog is that agent's system
// prompt: it is read once, before any work, by the thing about to act. Write
// entries as operating instructions — when to reach for this and not that, what
// an error obliges you to do next, what to do before you start — and not as a
// restatement of the flag name in a sentence.
package catalog

// Product is what wanctl is, in the owner's words. It heads the Markdown
// contract, because a reader meeting the command list for the first time needs
// to know what the list is a list of.
const Product = `wanctl is the external harness for a web AI. ` +
	`The AI in a chat window is the brain; wanctl gives it hands (exec, background jobs, read, edit, push/pull), ` +
	`eyes (command output, read, logs, screenshot), memory across turns (session rebind, job ledger) ` +
	`and safety rails (pairing, device identity, policy rules). Together they form one agent.`

// Type names for Param.Type. They are the JSON Schema types the MCP tools
// register, so changing one changes the wire schema.
const (
	TypeString = "string"
	TypeBool   = "boolean"
	TypeNumber = "number"
)

// Param is one argument, on either surface or both.
type Param struct {
	// Name is the MCP argument name. For a CLIOnly parameter it is the flag
	// name without dashes.
	Name string
	// CLI is the CLI spelling with its value placeholder, e.g.
	// "--target NS/DEV". Empty means "--" + Name. The sentinel NotOnCLI means
	// the parameter has no CLI equivalent.
	CLI string
	// Type is the JSON Schema type registered for the MCP tool.
	Type string
	// Required is the MCP schema's required flag. The CLI may still accept the
	// argument positionally.
	Required bool
	// Desc is the description both surfaces show.
	Desc string
	// CLIDesc replaces Desc for a CLI reader. It exists for the handful of
	// parameters that genuinely differ: `wanctl exec --script` names a local
	// file, while the MCP argument of the same name carries the source itself.
	CLIDesc string
	// CLIOnly keeps the parameter out of the MCP schema entirely.
	CLIOnly bool
	// MCPOnly keeps it out of the CLI parameter table.
	MCPOnly bool
}

// NotOnCLI marks a parameter that exists only as an MCP argument.
const NotOnCLI = "-"

// Failure is an error text a caller must recognise and act on, paired with
// what to do about it. These are matched on by agents, so the Text is the
// literal prefix the tool returns, not a paraphrase.
type Failure struct {
	Text  string
	Means string
}

// Group is one section of the command index. The order here is the order the
// index prints.
type Group string

const (
	GroupDevice     Group = "DEVICE LIFECYCLE"
	GroupSession    Group = "SESSION"
	GroupControl    Group = "CONTROL"
	GroupFiles      Group = "FILES"
	GroupOperations Group = "OPERATIONS"
)

// IndexGroups is the printed order of the index sections, with the one-line
// hint each section header carries.
var IndexGroups = []struct {
	Group Group
	Hint  string
}{
	{GroupDevice, "(run on the machine you want to control)"},
	{GroupSession, "(run where you — or your AI — drive from)"},
	{GroupControl, ""},
	{GroupFiles, ""},
}

// Command is one entry: a CLI subcommand, an MCP tool, or both.
type Command struct {
	// Name is the CLI spelling, e.g. "exec" or "trust server". Empty when the
	// primitive exists only as an MCP tool.
	Name string
	// MCPName is the MCP tool name, e.g. "wanctl_exec". Empty when the command
	// has no MCP tool.
	MCPName string
	// Group places the command in the index. Empty keeps it out of the index's
	// named sections; it still gets a help entry.
	Group Group
	// IndexLine is what the short index prints after the command name. Empty
	// keeps the command out of the index; it still gets a help entry and, if it
	// has no Group, a mention on the index's MORE line.
	IndexLine string
	// IndexRank orders a command inside its index group. Equal ranks keep
	// catalog order, which is MCP registration order and reads oddly for the
	// device commands — a person runs start before status.
	IndexRank int
	// Summary is the one-line headline of the help entry.
	Summary string
	// Desc is the full description. It is the MCP tool description verbatim, so
	// editing it here changes what every AI host reads.
	Desc string
	// CLINote is appended to Desc when a human reads `wanctl help`.
	CLINote string
	// MCPNote is appended to Desc when it is registered as an MCP tool
	// description.
	MCPNote string
	// Params are in MCP registration order; the CLI table follows the same
	// order.
	Params []Param
	// CLIExample is one command line. MCPExample is one tool call.
	CLIExample string
	MCPExample string
	// Errors are the failure texts a caller must react to.
	Errors []Failure
	// Handler names the MCP handler function. internal/mcp looks the handler up
	// by this key, so a typo fails registration at startup rather than silently
	// dropping a tool.
	Handler string
}

// MCPParams are the parameters that belong in the MCP schema, in registration
// order.
func (c Command) MCPParams() []Param {
	out := make([]Param, 0, len(c.Params))
	for _, p := range c.Params {
		if p.CLIOnly {
			continue
		}
		out = append(out, p)
	}
	return out
}

// CLIParams are the parameters a `wanctl help` table lists.
func (c Command) CLIParams() []Param {
	out := make([]Param, 0, len(c.Params))
	for _, p := range c.Params {
		if p.MCPOnly || p.CLI == NotOnCLI {
			continue
		}
		out = append(out, p)
	}
	return out
}

// CLIMeaning is the description a CLI reader gets.
func (p Param) CLIMeaning() string {
	if p.CLIDesc != "" {
		return p.CLIDesc
	}
	return p.Desc
}

// CLISpelling is how the parameter is typed on a command line.
func (p Param) CLISpelling() string {
	if p.CLI != "" {
		return p.CLI
	}
	return "--" + p.Name
}

// MCPDescription is what the MCP server registers: the shared description plus
// any MCP-only note.
func (c Command) MCPDescription() string {
	if c.MCPNote == "" {
		return c.Desc
	}
	return c.Desc + "\n\n" + c.MCPNote
}

// CLIDescription is what `wanctl help` prints: the shared description plus any
// CLI-only note.
func (c Command) CLIDescription() string {
	if c.CLINote == "" {
		return c.Desc
	}
	return c.Desc + "\n\n" + c.CLINote
}

// Lookup resolves a name a caller typed. Both spellings resolve, so an agent
// that only knows the MCP tool name can run `wanctl help wanctl_read`, and a
// multi-word CLI name ("trust server") resolves from its joined form.
func Lookup(name string) (Command, bool) {
	for _, c := range Commands {
		if c.Name == name || c.MCPName == name {
			return c, true
		}
	}
	return Command{}, false
}

// MCPCommands are the entries that register an MCP tool, in registration order.
func MCPCommands() []Command {
	out := make([]Command, 0, len(Commands))
	for _, c := range Commands {
		if c.MCPName != "" {
			out = append(out, c)
		}
	}
	return out
}
