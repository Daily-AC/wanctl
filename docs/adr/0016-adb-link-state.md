# 0016 — The agent reports its adb link to the app and the portal

Date: 2026-10-02
Status: accepted; implemented in v0.20.2

## Problem

The Android app's 提权通道 switch read "on" while elevation could not work at
all: the switch is the owner's consent, not a connection, and nothing below it
said whether wireless debugging was on or paired. The portal's ADB pairing
form said "配对成功" once and nothing afterwards. Owners took "on" for
"working" (owner's report, 2026-09-30).

## Decision

The agent is the only party that can tell, so it probes the adb channel itself
and reports one state to both surfaces:

| state       | meaning                                                         |
|-------------|-----------------------------------------------------------------|
| `off`       | 提权通道 is switched off in the app; nothing is probed           |
| `connected` | adbd ran `id` as shell                                          |
| `no_port`   | nothing answered: wireless debugging is off, or not found yet   |
| `unpaired`  | adbd answered and refused wanctl's key (never paired, or lapsed)|
| `confirm`   | adbd shows "Allow USB debugging?" for wanctl's key; tap Allow   |
| `error`     | anything else, with the reason (shown folded, for diagnosis)    |

- It probes when the app reports a new wireless-debugging port, right after a
  pairing, and otherwise once a minute (every five minutes in `confirm`,
  whose dialog every probe raises again, and in `error`). A probe never queues
  behind a running elevated command.
- **Portal:** console state gains an optional `adb` object
  (`{"state": …, "reason": …}`). An agent before v0.20.2 omits it and the
  portal shows only the pairing form, as before.
- **App:** with `--approvals-stdio` the agent prints `wanctl-adb {"state":…}`
  on stdout when the state changes. As with `wanctl-approval` lines, nothing a
  controller chose can start a line with that prefix.

## Not done

The su (root) channel is not probed in the background: on a rooted phone a
probe raises the root manager's consent dialog, which is exactly what the
switch being off is meant to prevent, and once on it would ask every minute.
