# Scoped command tokens

A scoped token is a credential for a program that needs exactly one thing from
a device — "restart the app on box A", "run the nightly backup" — and should
not be able to do anything else if it leaks or is tricked.

It can run **one literal command on one device**. Everything else is refused:
another command, the same command with a different argument, a pipe or `;`
appended, another device, files, logs, a shell, your account, other tokens.
It expires, and you can revoke it at any time.

> **Not available yet.** Scoped tokens need the next relay release (after
> v0.29.1), and devices updated to it. Until the relay you use is upgraded,
> `wanctl scoped-token` and the tokens it issues are refused.

## Issue one

With your normal login (`wanctl login`), on any machine:

```bash
wanctl scoped-token create --device home-pc --command 'systemctl restart app' --days 30 --label ci
```

The token (`wcs_…`) is printed once on stdout; the details go to stderr, so
`T=$(wanctl scoped-token create …)` captures just the token. It cannot be
shown again.

- `--device` must be one of your own devices, not a device shared to you.
- `--command` is matched byte for byte. One printable line, up to 4096 bytes.
  For anything longer, put it in a script on the device and scope the token to
  running the script.
- `--days` is 1–365, default 90. There is no non-expiring scoped token.
- You can hold at most 50 live scoped tokens.

```bash
wanctl scoped-token list          # id, state, command, device, label
wanctl scoped-token revoke 12     # its next request is refused
```

## Use one

Give the token to the program, set it as `WANCTL_TOKEN`, and run the command
with `--oneshot`:

```bash
WANCTL_TOKEN=wcs_… wanctl exec --target home-pc --oneshot -- 'systemctl restart app'
```

The machine using the token must trust the device's identity once, like any
controller: `wanctl trust server --target home-pc --fingerprint SHA256:…` with
the fingerprint the device prints when its agent starts.

Anything but that exact request is refused by the relay or by the device, and
nothing is executed. A revoked or expired token fails on the next request,
including on a session that was already open.

## What it does not do

A scoped token **narrows** access; it never adds any.

- The device's own mode, rules and approvals still decide whether the command
  runs. On a device that asks before running things, either approve the
  command once with a rule (`wanctl rules`) or the call waits for you like any
  other.
- The machine using the token still has to be paired with the device, the
  same as any controller. A stolen token used from a machine the device has
  never seen gets the device's pairing prompt, not a command.
- The device must run a wanctl version that supports scoped tokens. An older
  device refuses the token instead of ignoring the limit.
- It is not a secret-hiding tool: whoever holds the token can run that command.
  Pick commands that are safe to run on demand.

## How it is enforced

Sessions are end-to-end encrypted, so the relay cannot read the command. The
permission is therefore split. The relay admits the token only to its pinned
device, with exec-only rights, and tells the device which command was
authorised over the device's own authenticated channel. The device then
accepts only a request that is exactly "run this command once" and refuses
everything else, including any field it does not recognise. Changing the
client does not help: the rule is held by the device, and the relay re-checks
the token on every request. The design and the alternatives are in
ADR 0021 (`docs/adr/0021-scoped-command-token.md` in the repository).
