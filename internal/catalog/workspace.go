package catalog

func init() {
	Commands = append(Commands, Command{
		MCPName: "wanctl_workspace", Group: GroupSession,
		Summary: "Enter, inspect, cancel, or exit a remote workspace",
		Desc:    "Enter once with action='enter', target and an absolute project root. Save the returned workspace reference and pass it instead of target to wanctl_exec, wanctl_exec_async, wanctl_exec_poll, wanctl_read, wanctl_edit and wanctl_write. Relative file paths resolve against the project root; shell cwd and environment persist independently. Read AGENTS.md or CLAUDE.md and follow it before working. Each entry owns a separate shell, even under the same login. The reference survives MCP reconnects; it is not a credential and is never a global default for an account or a chat. A harness can inject it for its own conversation, but MCP cannot redirect a host's unrelated local tools.\n\nUse status to reconnect and inspect; exit explicitly closes the workspace and its shell. Network loss does not exit or cancel a received command. Cancel must name the active request_id and destroys the shell; collect its result, then explicitly exit and enter a new workspace. An expired/closed/invalid workspace MUST NOT silently fall back to local tools or another device. A device restart loses live shell state. Workspaces are not a filesystem sandbox; each operation still uses the existing device policy. Short-lived delegated credentials do not support persistent workspaces.\n\nLimits: 16 open workspaces per device; 128 command IDs (1 MiB total command text) and 8 MiB retained output per workspace; each command has the existing 30-minute execution limit. IDs are retained until exit, never evicted and then rerun. These are persistent command shells, not interactive PTYs. A program waiting on stdin is unsupported. No preview port forwarding or automatic host-tool replacement is included.",
		Params: []Param{
			{Name: "action", Type: TypeString, Required: true, MCPOnly: true, Desc: "enter | status | cancel | exit. Exit is the explicit end of this remote workspace."},
			{Name: "target", Type: TypeString, MCPOnly: true, Desc: "Device to enter. Omit when workspace is supplied."},
			{Name: "root", Type: TypeString, MCPOnly: true, Desc: "Absolute project directory on the device; required for enter."},
			{Name: "workspace", Type: TypeString, MCPOnly: true, Desc: "Exact reference returned by enter. Required for status/cancel/exit; also accepted by enter to recover a lost open response."},
			{Name: "request_id", Type: TypeString, MCPOnly: true, Desc: "The active request to cancel, or an existing request whose result status should include."},
		},
		MCPExample: "wanctl_workspace{\"action\":\"enter\",\"target\":\"lab\",\n    \"root\":\"/srv/app\"}", Handler: "mcpWorkspace",
	})
	for i := range Commands {
		c := &Commands[i]
		switch c.MCPName {
		case "wanctl_exec", "wanctl_exec_async", "wanctl_exec_poll", "wanctl_read", "wanctl_edit", "wanctl_write":
			c.Params = append(c.Params, Param{Name: "workspace", Type: TypeString, MCPOnly: true, Desc: "Remote workspace reference from wanctl_workspace. Mutually exclusive with target. Always carry it across turns/reconnects; never replace it with a guessed device. Relative file paths and explicit cwd are resolved on the device against its project root."})
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
			c.Params = append(c.Params, Param{Name: "request_id", Type: TypeString, MCPOnly: true, Desc: "Workspace only: unique 1-128 letters/digits/-/_ for this command. Generated if omitted and returned with the result. Reuse the SAME ID and unchanged command/cwd after an uncertain response; never invent a new ID to retry a possibly executed command."})
			c.Desc = "WORKSPACE MODE: with workspace, execution uses its independent persistent shell and returns JSON with request_id, done, code, output and next_offset. If done=false, call wanctl_exec_poll with workspace and job_id=request_id. Even after done=true, continue while next_offset < retained_bytes. Network loss only stops waiting. One command at a time; busy returns the current request_id. Matching sh/PowerShell scripts execute in the workspace shell itself: cd/export persist across calls. Short commands wait up to 250 ms on the device and need no extra poll connection. No oneshot or elevation in workspace mode.\n\n" + c.Desc
		case "wanctl_exec_poll":
			c.Desc = "WORKSPACE MODE: pass workspace instead of target and job_id equal to the request_id returned by workspace exec. JSON carries done, code, error, output and next_offset; reuse the offset to consume each page once. A closed device process has lost its ledger; do not silently start another command.\n\n" + c.Desc
		}
	}
}
