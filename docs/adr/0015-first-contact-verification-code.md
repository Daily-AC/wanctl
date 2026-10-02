# 0015 — First contact is checked with a device-shown code, not a fingerprint a human reads aloud

Date: 2026-09-28
Status: implemented on the feature branch; not released

## Problem

First contact is the one step in wanctl where the product asks a human to do
something hard, and does it at the worst possible moment.

A controller that has never dialled a device stops with `DEVICE IDENTITY
CONFIRMATION REQUIRED`, carrying the `SHA256:<43 base64 characters>` fingerprint
the device presented. The only way past it is for a human to compare that string
with the same string as shown on the device — the Android app's 连接详情 screen,
`wanctl id`, or the agent's startup banner — and then run `wanctl trust server
--target … --fingerprint …`.

Three things follow from that:

1. Forty-three base64 characters have no structure a person can chunk, and one
   wrong character is indistinguishable from an impersonation. The comparison
   decays into a glance at two screens.
2. Nothing tells the reader where the device's copy of the value lives. The
   refusal names a target and a fingerprint, not a place to look.
3. An AI-driven controller cannot do this part at all. It has no camera and no
   phone: the human has to move a 43-character string from a phone screen into a
   chat window by hand, which is where the conversation stalls and the human
   says "just connect" — the exact failure ADR 0002 named: "Whether that is a
   real defence now depends on people actually comparing them".

## Decision

Use an explicit two-step flow. The controller records the certificate it saw
before disclosing a fresh verification number; the human reads the resulting
nine-digit code from the device and reports it with that original fingerprint.
The controller never displays the expected answer.

- The first-contact refusal carries the canonical target, certificate
  fingerprint and a fresh six-digit **verification number**. It includes the
  device-side command and a trust command with a placeholder for the device's
  answer, never a prefilled verification code.
- On the device, `wanctl verify <number>` derives a nine-digit code from the
  verification number and that installation's **own certificate**, using the
  domain-separated `wanctl/verify/v1` SHA256 derivation. It names the number it
  answered. This is local: no relay, no agent, no network, just existing identity
  files. The Android app runs it from 连接详情 → 连接校验. A device without an
  identity refuses rather than creating one.
- On the controller, run:

  ```sh
  wanctl trust server --target X --fingerprint SHA256:... \
    --number N --code C
  ```

  Target, fingerprint and number must come from the **same first-contact
  refusal**; code comes only from the device's output. The controller re-dials,
  refuses unless the current fingerprint is exactly the original one, checks
  the code, and pins on a match. A wrong code returns `VERIFICATION CODE
  MISMATCH` without revealing the expected answer or writing a pin. A changed
  certificate returns `DEVICE IDENTITY MISMATCH`.
- There is no interactive trust path. A number requires both code and
  fingerprint. A code without a number is rejected, even if a fingerprint is
  also supplied. A missing proof cannot silently become fingerprint-only trust.
- Fingerprint-only pinning remains supported after an independent comparison
  of the whole fingerprint, including for devices older than `wanctl verify`.
- The MCP tool uses the same bound flow. Fingerprint is required in its schema;
  number and code are an optional pair. Its existing unsafe opt-in gate and
  host authorization requirements remain unchanged.

**Why the original fingerprint matters.** The short code is useful only while
bound to the certificate observed before the verification number was disclosed.
The second invocation must carry that fingerprint explicitly. Allowing a new
certificate to be chosen after disclosure would permit an attacker to search
for one whose code matches the real device's answer. A code check against only
whatever certificate answers the later dial is therefore insufficient.

**Why the number, and not a truncated fingerprint.** The relay already knows
the real device's fingerprint from its device records. A fixed short
fingerprint can be attacked before the human starts checking. Drawing the
number after observing a certificate and requiring that certificate again
prevents substituting an adaptively chosen certificate at confirmation. The
nine-digit code remains a comparison aid based on public inputs, not a secret
or a proof of possession. The human must obtain it independently on the device;
controller-side output must never make copying the expected answer a shortcut.

**What does not change.** The stored pin, the requirement for an explicit human
`--replace` decision after investigation, pairing, and policy checks remain.
First contact sends no application data to an unpinned endpoint. The number
reaches the device through the human and both derivations are local.

## Rejected alternatives

- **Truncate the fingerprint** (`transport.ShortFingerprint`): grindable, and it
  would replace a real check with a decoration.
- **Send a verification hello before pinning**, so the device can answer with a
  code bound to the dial: one comparison instead of two, but it breaks the
  documented "nothing was sent" guarantee and hands any attacker an
  unauthenticated way to make a device prompt its owner.
- **Let the device's pairing approval imply the pin**: rejected by ADR 0002, and
  on a device with auto-trust on (the Android app's default for a controller the
  owner has already allowed) the approval is silent, so it authenticates nothing.
- **Read the code from the portal's device record**: the seed and the comparison
  would come from the party the pin protects against.
- **A QR code**: a terminal has no scanner, and a phone cannot scan the value it
  is being asked to compare.

## Consequences

- A device older than this change shows no code; the controller's refusal still
  carries the fingerprint command, and an operator whose device cannot answer
  keeps the old flow. `wanctl trust server --fingerprint` therefore stays
  supported, not deprecated.
- The Android app gains one screen (连接详情 → 连接校验) that runs the bundled
  binary locally. It needs no new IPC: the app already runs short commands and
  reads their stdout.
- `wanctl verify` is CLI-only and deliberately absent from the MCP surface: it is
  run *on the device*, by a human standing there.
- The code is a comparison aid, not a secret and not a proof of possession: it is
  a function of the public certificate. What it proves is that *this* controller,
  holding the certificate it was just shown, and *that* device, printing from its
  own identity, derived the same value for a number the controller drew.

## Evidence

- `internal/transport/verifycode_test.go`: the derivation is a pure function of
  (fingerprint, number), moves with both inputs, refuses anything that is not a
  fingerprint or a six-digit number, and spreads across the digit space.
- `internal/client/verification_e2e_test.go`: a real relay and agent exercise
  refusal, device-side verification, successful bound pinning, wrong-code
  rejection, missing-fingerprint rejection, and refusal after identity changes.
  First-contact and mismatch errors do not disclose the expected answer.
- `verify_cli_test.go`: the same journey through the real binary in separate
  processes. The code comes exclusively from `wanctl verify` against the
  agent's own config directory; the controller carries the original fingerprint.
  Incomplete flags fail without a prompt or pin, and fingerprint-only trust
  remains supported.
- `internal/mcp/trustcode_test.go` and `trustcode_e2e_test.go`: the MCP refusal
  exposes the number, fingerprint and device-side command without the expected
  code, and the handler requires the complete bound verification inputs.
