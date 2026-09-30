# 0015 — Approvals on the owner's phone

Date: 2026-09-30
Status: accepted; implementation in progress (S13)

## Problem

A device in normal mode asks a human before it runs anything that no rule
covers. The only human it can ask is a console front-end: the portal while a
browser has the device open, or the device's own terminal. With neither, the
request is refused at once. In practice that means normal mode stops working
the moment the owner walks away from a computer, and the owner's four devices
ran 2,480 bypass-mode executions in the 30 days before this decision. The
portal is almost never opened on a phone (one Android API client in four days
of logs), and web push does not reach Chinese Android phones (S6: no
`PushManager` in the OEM browser, FCM unreachable from Chrome).

The S6 field trial on the owner's PGBM10 (ColorOS 14) showed that the wanctl
Android app's own long-lived connection delivers a notification in under a
second, even ten minutes into a locked screen, and that the owner, when not
expecting a request, picks the phone up 19 to 50 minutes later.

## Decision 1: one designated phone may approve for its owner

- The owner designates **one device per namespace** as the approval phone in
  the portal (device page, "设为审批手机"). It must be a device the namespace
  owns, and designation only succeeds if the device answers a test push, which
  only an agent hosted by the Android app does (Decision 3). The owner can
  withdraw it at any time; withdrawal takes effect at the next portal
  reconcile, and outstanding requests pushed to that phone can no longer be
  answered from it.
- While the phone is online, the portal keeps a resident console subscription
  to every online device the namespace owns (including the phone itself) and
  raises their approval wait to **180 seconds**. Each pending approval and each
  first-contact pairing request is pushed to the phone. This generalises the
  resident watch the Feishu workflow already had (`larkwatch.go`) and keeps its
  outlet separate: Feishu cards and the phone are two outlets of one watch.
- While the phone is offline — not live in the relay registry, which drops an
  agent 40 seconds after its last poll, or not answering a push — the portal
  stops watching and closes the resident sessions it opened, so the devices
  return to today's behaviour: a request nobody is watching is refused at once
  instead of hanging for three minutes. A push that fails denies that one
  request immediately for the same reason.
- The portal accepts a decision only when all of these hold: it arrives on the
  portal's own console session to the namespace's **current** approval phone;
  it names a push id the portal issued for that namespace; that id has not been
  answered before; and it is less than 24 hours old. The phone can therefore
  answer only what was shown to it, once. These records live in the portal's
  memory: a portal restart forgets them, and a decision naming one is then
  answered as gone, which fails safe.
- **Unlock is required.** The lock screen shows only "有 1 个待审批请求"
  (the notification's public version). The command appears after unlock: the
  detail screen is not allowed over the keyguard, and the notification's own
  "允许一次 / 拒绝" buttons require authentication.
- **Audit.** The target device's event log records the remote decision with
  `approver=phone:<phone device name>`, in the same line the portal browser
  path already writes (`portal:<email>`). A late approval (Decision 2) is
  logged when the grant is installed and when it is used.

The phone's device identity can now release requests on the owner's other
devices. This is the same power the portal's console session already has, and
it is bounded the same way: only the designated device, only pushed requests,
unlock first, revocable, and every use is in the target device's own log.

## Decision 2: a late approval releases the same command once within 30 minutes

The synchronous wait stays at 180 seconds. It catches the case where the owner
is holding the phone; waiting the 10-minute maximum would not catch the 19–50
minute case either and would only hang the controller longer. When the wait
runs out the request is refused as today, but the notification is not
withdrawn: it changes to "已过期，仍可批准：30 分钟内放行一次".

Approving it then makes the portal install a **one-shot grant** on the target
device: the same controller fingerprint, the same request kind and the same
`policy.CommandLabel` (for file requests, the same path) are allowed **once**
within **30 minutes**. The first matching request consumes it; anything else,
including the same command from another controller, asks again. Controllers
already retry refused commands, so the retry goes through. The grant lives in
the agent's memory only: an agent restart drops it, which fails safe. Refusing
an expired request only clears the notification.

Pairing requests do not get a late path. A device keeps an unanswered pairing
for five minutes (`pairTTL`); after that the phone says the request is gone and
the controller has to connect again. Trusting a key permanently on the strength
of a stale prompt is not worth the convenience.

## Decision 3: the app and the agent talk over the child process's stdio

The Android app already runs the Go agent as a child process and reads its
stdout. It passes a new flag, `--approvals-stdio`, and keeps the child's stdin
open (it used to close it). With the flag:

- The portal sends `approval_push` over its console session. The agent writes
  the card as one stdout line, `wanctl-approval {json}`, and acknowledges the
  RPC. The app turns the line into a notification and never writes it to its
  log file.
- A decision is one stdin line, `{"id":"…","verdict":"y"|"n"}`. The agent sends
  it to the portal as an unsolicited `approval_reply` on its console session,
  and keeps it in a small outbox until the portal pushes a final state for that
  id, resending it whenever a new console session opens. A decision made while
  the phone had no network is therefore delivered when it reconnects, and
  arrives as a late approval if the request has expired meanwhile.

Without the flag, `approval_push` is refused, which is how the portal tells a
phone running the app from any other device. No new port, socket, Android IPC
surface, FCM or vendor push is involved; the approval travels on the
connection the phone already keeps. The agent prints controller-supplied names
quoted, so a controller cannot forge a `wanctl-approval` line, and a forged
card could only produce a decision for an id the portal never issued, which it
discards.

## Protocol additions

- `approval_push` (portal to device, RPC): `Data` is the card — `id`, `state`
  (`pending`, `expired`, `done`, `test`), `kind`, `device`, `peer` (controller
  name), `peer_fp`, `cmd`, `path`, `cwd`, `created`, `expires`, and for `done`
  a `result` (`allowed`, `denied`, `granted`, `handled`, `gone`).
- `approval_reply` (device to portal, unsolicited): `ApprovalID`, `Verdict`.
- `grant_once` (portal to device, RPC): `RuleKind`, `Pattern` (the label or
  path), `FP` (controller), `TimeoutSec` (1800), `Approver`.

An older agent answers the new RPCs with "unknown RPC kind": it cannot be
designated as a phone, and a late approval for it reports the request as gone.

## Not done

Web push, a portal approval page for phones, a web notification inbox, iOS,
Feishu approval, pushing other events to the phone, and a notify-only app mode.
