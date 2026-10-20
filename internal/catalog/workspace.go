package catalog

// This contract is enabled only by the explicit stdio conversation mode.
const WorkspaceSessionInstructions = `WORKSPACE CONVERSATION MODE
This MCP process belongs to ONE conversation. Never share it across chats.
Enter once with wanctl_workspace(action="enter", target=..., root=...).
If the host supplied WANCTL_WORKSPACE, that reference is already bound.
Exec, read, edit, write and polling then use that workspace automatically.
Explicitly exit before entering a different workspace. Errors never change
where work runs. After restarting this MCP process, attach with the saved
workspace reference; attach does not create a new shell or restore lost state.
The connection is reused, but device trust, relay authorization and policy
are rechecked on each operation. Cancel/exit use an independent connection.
This mode controls these MCP tools, not a host's unrelated local tools.
Before working, read the project AGENTS.md or CLAUDE.md and follow it.`

func init() {
	Commands = append(Commands, Command{
		Name: "workspace", MCPName: "wanctl_workspace",
		IndexLine: "enter, resume or exit a persistent workspace",
		Summary:   "Enter, inspect, cancel, or exit a remote workspace",
		Desc:      "A persistent project shell on a device. Enter once with action='enter', target and an absolute root; then pass the returned workspace reference instead of target to wanctl_exec, wanctl_exec_async, wanctl_exec_poll, wanctl_read, wanctl_edit and wanctl_write. cwd and env persist, and relative paths resolve against the root. Keep the reference across turns and reconnects; if it turns invalid or closed, never fall back to another device or to local tools. Read AGENTS.md or CLAUDE.md in the root and follow it before working.\n\nattach re-checks a saved reference after a restart; status inspects; cancel needs the active request_id and destroys the shell; exit closes it. It is not a sandbox: device policy still applies. Programs that read stdin are unsupported. Limits: 16 workspaces per device, 128 commands and 8 MiB of output each, 30 minutes per command.",
		Params: []Param{
			{Name: "action", CLI: "enter|attach|status|poll|cancel|exit", Type: TypeString, Required: true, Desc: "enter | attach | status | cancel | exit.", CLIDesc: "Lifecycle action (first positional argument). Poll collects one output page; it does not wait for completion."},
			{Name: "target", CLI: "--target DEVICE", Type: TypeString, Desc: "Device to enter."},
			{Name: "root", CLI: "--root PATH", Type: TypeString, Desc: "Absolute project directory on the device; required for enter."},
			{Name: "workspace", CLI: "--workspace REF", Type: TypeString, Desc: "The reference enter returned; required for every other action. enter accepts it to recover an uncertain open.", CLIDesc: "Exact reference returned by enter. Defaults to WANCTL_WORKSPACE. Enter accepts it to recover an uncertain open; attach only inspects it."},
			{Name: "request_id", CLI: "--request-id ID", Type: TypeString, Desc: "The active request, for cancel; or a request whose state status should include."},
			{Name: "offset", CLI: "--offset N", Type: TypeNumber, CLIOnly: true, Desc: "Poll only: byte offset returned as next_offset by the previous result."},
		},
		CLINote:    "Use `workspace enter --target DEVICE --root /absolute/project` to create a workspace. Lifecycle commands return JSON. Save its workspace value verbatim, then pass --workspace REF to exec/read/edit/write or export WANCTL_WORKSPACE in this terminal or harness environment. No account-wide default is saved. `workspace attach` checks an existing reference; it does not change the parent shell environment. `workspace poll --request-id ID [--offset N]` returns one JSON output page. Cancel requires the active request ID and invalidates its shell. Exit closes it; unset WANCTL_WORKSPACE afterwards. Never silently discard an unavailable reference.\n\nA reference is owned by the controller identity: CLI and local stdio with the same config can resume each other's workspaces. Hosted OAuth MCP has a different controller identity and must enter its own workspace even for the same account. Files on the device remain shared according to the existing permissions.",
		CLIExample: "wanctl workspace enter --target lab --root /srv/app\n  export WANCTL_WORKSPACE='<workspace from the JSON result>'\n  wanctl exec 'export MODE=test; cd src'\n  wanctl exec 'printf \"%s\\n\" \"$MODE\"; pwd'\n  wanctl read README.md\n  wanctl workspace exit\n  unset WANCTL_WORKSPACE",
		MCPExample: "wanctl_workspace{\"action\":\"enter\",\"target\":\"lab\",\n    \"root\":\"/srv/app\"}", Handler: "mcpWorkspace",
	})
	for i := range Commands {
		c := &Commands[i]
		switch c.MCPName {
		case "wanctl_exec", "wanctl_exec_async", "wanctl_exec_poll", "wanctl_read", "wanctl_edit", "wanctl_write":
			c.Params = append(c.Params, Param{Name: "workspace", CLI: "--workspace REF", Type: TypeString, MCPOnly: c.Name == "", Desc: "Reference from wanctl_workspace, instead of target.", CLIDesc: "Remote workspace reference; defaults to WANCTL_WORKSPACE in this caller's environment. Mutually exclusive with target. Relative paths resolve against its root. An unavailable workspace never falls back to legacy execution."})
			for j := range c.Params {
				if c.Params[j].Name == "target" {
					c.Params[j].Required = false
					c.Params[j].Desc += " Omit with workspace."
				}
			}
		}
		switch c.MCPName {
		case "wanctl_exec", "wanctl_exec_async":
			c.Params = append(c.Params, Param{Name: "request_id", CLI: "--request-id ID", Type: TypeString, MCPOnly: c.Name == "", Desc: "Workspace only; generated if omitted. After an uncertain response retry with the SAME id and command, never a new one."})
			if c.Name == "exec" {
				c.Params = append(c.Params, Param{Name: "async", Type: TypeBool, CLIOnly: true, Desc: "Workspace only: submit and return JSON immediately. Collect output with workspace poll --request-id ID. Without this flag exec waits, prints output and propagates the remote exit code; Ctrl-C stops waiting without cancelling the remote command."})
			}
		}
	}
}
