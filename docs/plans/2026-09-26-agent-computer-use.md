# Agents that operate a remote desktop through wanctl

Status: idea, recorded 2026-09-26. Nothing built. Android is out of scope for
now and will be handled separately.

## The owner's ask

> I want something like computer use, where an agent operates my devices
> through wanctl. People can control a device smoothly with a remote desktop
> app like UU Remote; can an agent do the same?

## What wanctl has today

- `screenshot`: one full-screen PNG on Windows (.NET), macOS (`screencapture`)
  and Linux (grim, gnome-screenshot or import), gated like `exec`
  (`internal/server/screenshot.go`).
- `exec`, `read`, `write`, `edit`, `push`, `pull`.
- No mouse or keyboard input on any desktop platform, and no way to read the
  UI element tree.
- `exec` streams output but takes no streaming input, so a long-lived
  device-side process cannot be driven interactively.

## Smooth for an agent is different from smooth for a person

A person using a remote desktop needs 30–60 frames per second and an input
echo under 100 ms. An agent works in steps: look at the screen, think, act,
look again. The model's thinking takes seconds per step and dominates. What
makes the agent fast and reliable is:

1. **Low overhead per step.** Today every wanctl command pays session setup
   plus several relay round trips: about 0.7 s for an `exec` between two
   machines at home through the Hong Kong relay (about 106 ms per round trip).
   A persistent session that carries one round trip per action brings that to
   about 0.1 s through the relay, and a few milliseconds on a direct path on
   the same LAN.
2. **Targeting by control, not only by pixel.** Clicking "the Save button"
   through the accessibility tree (UIA on Windows, AX on macOS) survives DPI,
   layout and theme changes that break pixel coordinates.
3. **Working in the background.** The agent should be able to act on one
   window while the owner keeps using the machine, without stealing the mouse
   or focus.

A live view for a person to watch or take over is where UU-style smoothness
matters. That is a separate, later piece, and the one where a direct (P2P)
path pays off most.

## Do not build the driver: use cua-driver

`trycua/cua`'s `cua-driver` (MIT, Rust, releases for Windows, macOS and
Linux x86_64/arm64; v0.29.1 on 2026-09-25) is already a computer-use driver
for agents:

- It speaks MCP over stdio (`cua-driver mcp`), so any agent that knows MCP can
  use it with no client code.
- It targets by accessibility tree or by pixel, and delivers input in the
  background where the OS allows, refusing with an exact code where it does
  not. Its published action matrix (`libs/cua-driver/docs/action-support.md`)
  lists accepted runs on Windows/Win32, macOS/Quartz, Linux X11 and Sway.
- It has permission modes (`standard`, `bounded` with a reviewed manifest,
  `unrestricted`) and a cursor overlay that shows where the agent is acting.
- The optional perception extension is AGPL. We would not ship it.

Other prior art, for design reference only: `CursorTouch/Windows-MCP` (MIT,
Python, Windows), `microsoft/UFO` (MIT, Python, Windows UIA agent),
`bytedance/UI-TARS-desktop` (Apache-2.0, TypeScript), `go-vgo/robotgo`
(Apache-2.0, Go input library, cgo), `rustdesk/rustdesk` (AGPL, remote
desktop; design reference only).

We have not run cua-driver ourselves yet. Everything above comes from its
repository.

## What wanctl would add

wanctl's part is to get a driver on a remote machine safely into a local
agent's hands, across NAT, with the trust and approval model it already has.

1. **A remote stdio MCP bridge.** The controller's `wanctl mcp` exposes the
   device's `cua-driver mcp` tools (namespaced per device), or a new command
   bridges one device-side stdio MCP server to a local stdio. This needs a new
   session kind: a long-lived, two-way byte stream to one allow-listed program
   on the device, not a general interactive shell.
2. **Getting the driver onto the device.** The agent fetches the cua-driver
   release for its platform on first use, checks its checksum, and keeps it
   next to the wanctl binary. Downloads from GitHub are slow from China, so
   the relay mirrors it the way it mirrors wanctl's own releases. It is not
   compiled into wanctl, so the wanctl binary does not grow.
3. **Running in the owner's desktop session.** On Windows the agent already
   runs as a logon task in the interactive session. A machine whose agent runs
   in session 0 (as the 5090's managed agent does today) cannot see or drive
   the desktop and would need to change. On macOS the owner grants
   Accessibility and Screen Recording once.
4. **Safety.**
   - Gated like `exec` at least, because a controller allowed to run commands
     could already start the driver itself.
   - A visible sign on the device while it is being controlled, from the
     driver's cursor overlay or wanctl's own tray/notification.
   - A local stop that works within a second: a hotkey or the portal.
   - A log of every action with before/after screenshots, kept on the device.
   - Whether a control session should always need a fresh approval, even in
     bypass mode, is an open question for the owner.

## First milestone and its acceptance (to be confirmed before building)

Devices: the owner's Mac and one Windows machine whose agent runs in the
interactive session.

- Claude Code, connected only through wanctl, completes two tasks on each
  device with no human input: (a) open the system text editor, type a given
  sentence, save it to a given path, then `wanctl read` shows exactly that
  sentence; (b) in a small GUI test app, find and press a named button, and
  the app's state shows it was pressed.
- Median wanctl overhead per action, over the relay, is at most 0.3 s beyond
  the driver's own time, measured over at least 30 actions.
- While the agent works in the background, the owner can keep typing in
  another window without keystrokes going astray, on the actions cua-driver
  lists as background-capable.
- The owner can stop a running control session within one second.
- The wanctl binary does not grow.

## Open questions for the owner

1. Must the agent work in the background while you use the machine, or is it
   acceptable for it to take over the screen?
2. Should a control session always ask you for approval, even on a device in
   bypass mode?
3. Is a live view for you to watch and take over part of the first version?
4. Which Windows machine is the first target, given the 5090's agent runs in
   session 0?

## Next step

Run cua-driver by hand on the Mac and a Windows machine before any wanctl
code: install it, drive it from a local MCP client, and check the background
and accessibility claims on real apps. If it holds up, write the design for
the stdio bridge.
