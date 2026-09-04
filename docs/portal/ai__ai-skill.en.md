# Let your AI control a device

Make an AI (Claude Code, say) the controller, and approve its work in the
browser. Two steps:

**a.** Tell your AI one line:

> Install https://relay.example.com/skills

The AI fetches that URL, gets the wanctl skill, and writes it to
`~/.claude/skills/wanctl/SKILL.md` on its own machine. Restart it and the skill
is live.

> Give the AI the **relay** origin (`relay.example.com`), not the portal
> origin. The portal origin goes through a browser login, so an AI fetching it
> directly hits the login page and never sees the skill.

**b.** Have the AI run `wanctl login` once: a browser opens for you to sign in
to the portal (GitHub account) and hand back a one-time code. Paste it to the
AI and press enter — that machine now carries your identity and can use
`wanctl exec/push/pull` on the devices you granted it.

> The first time the AI reaches a device, that device's **Waiting** page raises
> a pairing request carrying the name the AI gave for itself; click **Trust
> it**. Every command it sends afterwards still waits there for you, unless you
> switch the device to **Allow all** or pre-authorize it under **Rules**.
