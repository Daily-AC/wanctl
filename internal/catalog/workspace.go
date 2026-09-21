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
		Desc:      "Enter once with action='enter', target and an absolute project root. Save the returned workspace reference and pass it instead of target to wanctl_exec, wanctl_exec_async, wanctl_exec_poll, wanctl_read, wanctl_edit and wanctl_write. Relative file paths resolve against the project root; shell cwd and environment persist independently. Read AGENTS.md or CLAUDE.md and follow it before working. Each entry owns a separate shell, even under the same login. The reference survives MCP reconnects; it is not a credential and is never a global default for an account or a chat. A harness can inject it for its own conversation, but MCP cannot redirect a host's unrelated local tools.\n\nUse attach to bind an existing workspace after restarting a dedicated conversation process; it never creates a shell. Use status to inspect; exit explicitly closes the workspace and its shell. Network loss does not exit or cancel a received command. Cancel must name the active request_id and destroys the shell; collect its result, then explicitly exit and enter a new workspace. An expired/closed/invalid workspace MUST NOT silently fall back to local tools or another device. A device restart loses live shell state. Workspaces are not a filesystem sandbox; each operation still uses the existing device policy. Short-lived delegated credentials do not support persistent workspaces.\n\nLimits: 16 open workspaces per device; 128 command IDs (1 MiB total command text) and 8 MiB retained output per workspace; each command has the existing 30-minute execution limit. IDs are retained until exit, never evicted and then rerun. These are persistent command shells, not interactive PTYs. A program waiting on stdin is unsupported. No preview port forwarding or automatic host-tool replacement is included.",
		Params: []Param{
			{Name: "action", CLI: "enter|attach|status|poll|cancel|exit", Type: TypeString, Required: true, Desc: "enter | attach | status | cancel | exit. Exit is the explicit end of this remote workspace.", CLIDesc: "Lifecycle action (first positional argument). Poll collects one output page; it does not wait for completion."},
			{Name: "target", CLI: "--target DEVICE", Type: TypeString, Desc: "Device to enter. Omit when workspace is supplied."},
			{Name: "root", CLI: "--root PATH", Type: TypeString, Desc: "Absolute project directory on the device; required for enter."},
			{Name: "workspace", CLI: "--workspace REF", Type: TypeString, Desc: "Exact reference returned by enter. Required for attach/status/cancel/exit outside conversation mode; also accepted by enter to recover a lost open response.", CLIDesc: "Exact reference returned by enter. Defaults to WANCTL_WORKSPACE. Enter accepts it to recover an uncertain open; attach only inspects it."},
			{Name: "request_id", CLI: "--request-id ID", Type: TypeString, Desc: "The active request to cancel, or an existing request whose result status should include."},
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
			c.Params = append(c.Params, Param{Name: "workspace", CLI: "--workspace REF", Type: TypeString, MCPOnly: c.Name == "", Desc: "Remote workspace reference from wanctl_workspace. Mutually exclusive with target. Always carry it across turns/reconnects; never replace it with a guessed device. Relative file paths and explicit cwd are resolved on the device against its project root.", CLIDesc: "Remote workspace reference; defaults to WANCTL_WORKSPACE in this caller's environment. Mutually exclusive with target. An unavailable workspace never falls back to legacy execution."})
			for j := range c.Params {
				if c.Params[j].Name == "target" {
					c.Params[j].Required = false
					c.Params[j].Desc += " Omit when workspace is supplied."
				}
				if c.Params[j].Name == "path" {
					c.Params[j].Desc += " With workspace, relative paths are based on its project root."
				}
			}
		}
		switch c.MCPName {
		case "wanctl_exec", "wanctl_exec_async":
			c.Params = append(c.Params, Param{Name: "request_id", CLI: "--request-id ID", Type: TypeString, MCPOnly: c.Name == "", Desc: "Workspace only: unique 1-128 letters/digits/-/_ for this command. Generated if omitted and returned with the result. Reuse the SAME ID and unchanged command/cwd after an uncertain response; never invent a new ID to retry a possibly executed command."})
			if c.Name == "exec" {
				c.Params = append(c.Params, Param{Name: "async", Type: TypeBool, CLIOnly: true, Desc: "Workspace only: submit and return JSON immediately. Collect output with workspace poll --request-id ID. Without this flag exec waits, prints output and propagates the remote exit code; Ctrl-C stops waiting without cancelling the remote command."})
			}
			c.Desc = "WORKSPACE MODE: with workspace, execution uses its independent persistent shell and returns JSON with request_id, done, code, output and next_offset. If done=false, call wanctl_exec_poll with workspace and job_id=request_id. Even after done=true, continue while next_offset < retained_bytes. Network loss only stops waiting. One command at a time; busy returns the current request_id. Matching sh/PowerShell scripts execute in the workspace shell itself: cd/export persist across calls. Short commands wait up to 250 ms on the device and need no extra poll connection. No oneshot or elevation in workspace mode.\n\n" + c.Desc
		case "wanctl_exec_poll":
			c.Desc = "WORKSPACE MODE: pass workspace instead of target and job_id equal to the request_id returned by workspace exec. JSON carries done, code, error, output and next_offset; reuse the offset to consume each page once. A closed device process has lost its ledger; do not silently start another command.\n\n" + c.Desc
		}
	}
}
