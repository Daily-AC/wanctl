# First-contact verification code, 2026-09-28

Issue: [#129](https://github.com/Daily-AC/wanctl/issues/129). Decision record:
[ADR 0015](../adr/0015-first-contact-verification-code.md).

> Historical implementation plan. The security review superseded its interactive
> flow and controller-displayed code: confirmation now requires the original
> fingerprint together with the number and the code read on the device. The
> controller never prints the expected answer. See ADR 0015 for the current design.

## Why

First contact asked a human to compare a 43-character fingerprint between a
device screen and a terminal. Two consequences showed up in practice, both
reported in the issue: nobody knows where the device's copy of the value lives,
and an AI-driven controller cannot move that string at all. The step that is
supposed to make TOFU meaningful was the step people skipped.

## Design, in one paragraph

The controller draws a six-digit verification number per dial and derives a
nine-digit code from it and the certificate the dial presented. The device
derives the same code locally from its own certificate with `wanctl verify
<number>` (the Android app runs that command from 连接详情 → 连接校验). The
controller checks whatever code it is handed against the certificate answering
now, and pins only on a match. Nothing is sent to an unpinned endpoint: the
number travels through the human, exactly as the fingerprint did, and both
derivations are local, so no relay or portal can forge a match.

## Real run

A relay, an agent and a controller, each with its own config dir, through the
built binary, one process per command:

```
----- controller: exec (first contact)
  fingerprint:          SHA256:d7McFG6z0Xe9bbXKLNqodlXzHngNhJ0GtkNYjkMd1JQ=
  verification number:  012 300
  verification code:    855 453 452
On the device, run `wanctl verify 012300` (Android app: 连接详情 → 连接校验) and check the code it
prints matches the code above. Then run:
  wanctl trust server --target "alice/6b1be4d4-1cd3-4d01-83a8-fdc98fa112c2" --number 012300 --code 855453452
A device whose wanctl predates `wanctl verify` can only be checked by fingerprint:
  wanctl trust server --target "alice/6b1be4d4-1cd3-4d01-83a8-fdc98fa112c2" --fingerprint "SHA256:d7McFG6z0Xe9bbXKLNqodlXzHngNhJ0GtkNYjkMd1JQ="
----- device: wanctl verify 012300
verification code: 855 453 452
  for number:  012 300
  this device: SHA256:d7McFG6z0Xe9bbXKLNqodlXzHngNhJ0GtkNYjkMd1JQ=
Compare the code above with the one printed by the controller that shows the
same number. A controller showing a different code is not talking to this device.
----- controller: trust server with a code nobody read off the device
wanctl: VERIFICATION CODE MISMATCH for "demo-pc"
  verification number:     012 300
  this controller derived: 855 453 452  (from the certificate that dial presented)
  read off the device:     000 000 000
Nothing was pinned, and the identity was NOT recorded. Either the device is not the
machine this number was issued for, or that code belongs to a different number — a
number from an earlier command is not this one. Run `wanctl verify 012300` on the device
and read the code printed for THAT number; do not retry this call as is.
(exit 1)
----- controller: trust server with the device's own code
pinned device "alice/6b1be4d4-1cd3-4d01-83a8-fdc98fa112c2" identity SHA256:d7McFG6z0Xe9bbXKLNqodlXzHngNhJ0GtkNYjkMd1JQ=
----- controller: exec again
hi
```

The same check with no flags at all, driven through a real pty (the number here
is this attempt's, so the code has to be read off the device for *that* number):

```
first contact with "alice/8476debe-955d-498f-a387-28e414899359" — nothing is pinned yet
  fingerprint:         SHA256:0cHS/QWsKGv/rI4rh9/tkp7yL9hRrqW/xq5AOe1scaw=
  verification number: 692 049
On that device run `wanctl verify 692049` (Android app: 连接详情 → 连接校验),
then type the code it prints.
verification code: 854318685
pinned device "alice/8476debe-955d-498f-a387-28e414899359" identity SHA256:0cHS/QWsKGv/rI4rh9/tkp7yL9hRrqW/xq5AOe1scaw=
```

## Android acceptance, on a real Android system

The device side is an Android app, so the screen and the sandbox were exercised
on a booted AVD (`Orca_Pixel7_API34`, arm64-v8a, API 34, 1080x2400), with the app
built from this branch by `scripts/build-apk.sh dev`, a local relay on the host
(`WANCTL_TOKENS=demo-token:alice`), and the phone enrolled against it with
自动信任新控制端 and 自动放行所有命令 on.

The controller's first contact with the phone:

```
wanctl: DEVICE IDENTITY CONFIRMATION REQUIRED for "alice/5362d55e-7404-4411-a347-aa49229cc634"
  fingerprint:          SHA256:I9REDPH+IqHP8+qs1LxdrMB0kXxLMEaWYQvd5GDRdW4=
  verification number:  114 602
  verification code:    930 665 463
```

What the phone's 连接详情 → 连接校验 screen showed after 114602 was typed in and
计算校验码 tapped (uiautomator dump of the live view hierarchy, verbatim):

```
text="本机校验码：930 665 463
与控制端显示的那一串逐位核对：一致才继续，不一致就是对面连的不是这台设备。"
```

The two agreeing is the whole flow. Then, on the controller:

```
$ wanctl trust server --target 5362d55e-… --number 114602 --code 930665463
pinned device "alice/5362d55e-…" identity SHA256:I9REDPH+IqHP8+qs1LxdrMB0kXxLMEaWYQvd5GDRdW4=
$ wanctl exec --target 5362d55e-… "getprop ro.product.model; getprop ro.build.version.release"
sdk_gphone64_arm64
14
```

Three cross-checks were made on that number, so the agreement is not one
implementation agreeing with itself:

| Source | Fingerprint | Code for 114602 |
| --- | --- | --- |
| the app's own `files/wanctl/cert.pem`, hashed here | `SHA256:I9REDPH+…dW4=` | — |
| the app's screen | `SHA256:I9REDPH+…dW4=` | `930 665 463` |
| an independent derivation of the documented one-liner | — | `930 665 463` |
| the controller's `transport.VerifyCode` | — | `930 665 463` |

The 连接详情 screen also renders the new 身份核对 group and its note, and the
app's own `wanctl verify` path is what produced the code: the app runs the
bundled binary with its own `WANCTL_CONFIG_DIR`, so this exercised the Android
config-directory handling inside the app sandbox, not the desktop layout.

## Files changed

| File | What |
| --- | --- |
| `internal/transport/verifycode.go` | `NewVerifyNumber`, `VerifyCode`, `NormalizeVerifyNumber`, `NormalizeVerifyCode`, `GroupDigits`; rejection-sampled derivation, no modulo bias |
| `internal/transport/verifycode_test.go` | determinism, both inputs matter, refusals, freshness, digit spread, grouping |
| `internal/transport/identity.go` | `LoadIdentity`: read the identity, never mint one |
| `internal/client/client.go` | `TrustRequiredError` carries number and code; `TrustChallenge`, `ChallengeTrust`, `ConfirmTrust`, `present`; `VerifyCodeMismatchError`, `VerifiedIdentityChangedError` |
| `internal/client/verification_e2e_test.go` | the flow over a real relay and agent, including a reinstalled device and a carried number |
| `main.go` | `cmdVerify`; `trust server --number/--code`, the interactive challenge, `--fingerprint` kept |
| `verify_cli_test.go` | the same journey through the binary in separate processes, plus `wanctl verify` on the device's own config dir |
| `internal/mcp/server.go` | first-contact text carries the number/code and the device-side command; `mcpTrustServer` takes `number`/`code`, `fingerprint` optional, refuses a pin nothing was verified for |
| `internal/mcp/trustcode_test.go` | HTTP/stdio wording; handler validation |
| `internal/mcp/trustcode_e2e_test.go` | the whole first contact through tool calls against a real device: refusal, device code, refusal of a fabricated one, pin, command runs |
| `internal/mcp/testdata/mcp_registration.json` | re-captured: three optional params on `wanctl_trust_server` |
| `internal/catalog/commands.go`, `docs/contract.md` | `verify` entry; `trust server` params, examples and the new refusal |
| `internal/catalog/instructions.go` | the refusal line now names both ways to resolve it |
| `android/.../MainActivity.java` | 连接详情 → 连接校验 runs the bundled `wanctl verify` locally |
| `docs/device-identity.md`, `docs/android.md` + `.zh.md` | the flow, and where the code is shown on a phone |

## Acceptance (written before implementation)

| # | Criterion | Result | Verdict |
| --- | --- | --- | --- |
| 1 | First contact over a real relay prints a number and a code, and the code matches what the device derives from its own certificate | `TestFirstContactOffersAVerificationCodeOnlyTheDeviceCanAnswer`; CLI transcript above | pass |
| 2 | A code that did not come off the device pins nothing | `VERIFICATION CODE MISMATCH`, `trust servers` still `pinned devices: 0` | pass |
| 3 | The code read off the device pins it, and the device is then drivable | transcript above; `exec` prints `hi` | pass |
| 4 | A device reinstalled between the number and the confirmation is refused as an identity change, not pinned | `TestConfirmingADeviceThatChangedAfterTheNumberWasIssuedPinsNothing` | pass |
| 5 | A number carried in from an earlier refusal is checked against the certificate answering now | `TestACarriedNumberIsCheckedAgainstTheCertificateAnsweringNow` | pass |
| 6 | Nothing is sent to an unpinned endpoint: the dial reads the certificate and closes | `present` sends no hello (`internal/client/client.go`); `TestHandshakeOverPipe` still asserts a first contact persists nothing | pass |
| 7 | `wanctl verify` is local: no relay, no agent, no token | `TestVerifyPrintsTheCodeAControllerDerivesFromTheSameCertificate` runs with `WANCTL_RELAY=`/`WANCTL_TOKEN=` empty | pass |
| 8 | `wanctl verify` never mints an identity it was only asked about | `TestVerifyRefusesToMintAnIdentityItWasOnlyAskedAbout` | pass |
| 9 | The old `--fingerprint` path still works, unchanged | `TestCmdTrustServerPinsVerifiedIdentity`, `TestMCPTrustServerPinsExactTarget` | pass |
| 10 | An MCP caller can resolve first contact from the user's spoken code, and is refused when nothing was verified | `internal/mcp/trustcode_test.go` | pass |
| 11 | Instructions keep their budget and every indexed command has an entry | `TestInstructionsFitTheBudget`, `TestIndexFitsTheBudget`, `TestEveryIndexedCommandHasAnEntry` | pass |
| 12 | The Android app still builds and still speaks the contract its Go side keys on | `scripts/build-apk.sh dev`; `TestAgentErrorsTheAppKeysOn` | pass |
| 13 | With no flags on a terminal, the same check runs as one exchange and pins only on a match | the pty transcript above (exit 0, then `exec` succeeds) | pass |
| 15 | The Android app computes the code on the device and the controller accepts it | the AVD run above: 930 665 463 on both sides, then `exec` returned `sdk_gphone64_arm64` / `14` | pass |
| 16 | A new controller and an old device, and an old controller and a new device, both still complete first contact | the cross-version runs above | pass |
| 17 | The MCP surface can finish the whole journey, not just describe it | `TestMCPFirstContactEndsInAPinAndTheCommandRuns` | pass |
| 14 | Without a terminal, or with a number and no code, the command fails closed and pins nothing | `TestTrustServerWithoutAVerificationPinsNothing` | pass |

## Cross-version, both directions

A device is updated on its own schedule, so the two old/new pairings were run
against real binaries: the pre-change one built from `main` (the released
behaviour), the post-change one from this branch.

| Pair | Observed |
| --- | --- |
| new controller → old device | the refusal carries the number, the code, and the fingerprint line, plus "A device whose wanctl predates `wanctl verify` can only be checked by fingerprint"; `trust server --fingerprint` pinned it and `exec` returned `from-old-device` |
| old controller → new device | unchanged: the old refusal text, `trust server --fingerprint` pinned it, `exec` returned `from-new-device` |
| old binary asked to verify | `unknown command "verify"` — which is exactly why the refusal names the fingerprint fallback |

Nothing on the device side changed for pairing or authorization, so an old
controller meeting a new device takes the path it always took.

## Review round 1

| Finding | Change | Test |
| --- | --- | --- |
| `ConfirmTrust` compared the typed target with the resolved canonical name, so `--fingerprint` alongside `--number/--code` on an unqualified target (`--target home-pc`) raised a false `DEVICE IDENTITY MISMATCH`, printing the same value twice | the check is the certificate only; `PinServer` resolves the target when it writes the pin | `TestAChallengeConfirmsThroughANonCanonicalTarget` |
| `--code` without `--number` was silently dropped: on a terminal it fell through to the interactive prompt, off one it failed with unrelated usage text, while the MCP handler refused the same input | refused where the other flag validation lives, naming the missing flag | `TestTrustServerWithoutAVerificationPinsNothing` / `code without a number` |
| Nothing pinned the derivation itself — every test derived both sides with the same helper, so a changed domain string, separator or digit count would keep the suite green while two versions stopped agreeing | a known-answer vector, reproduced through the built binary and by an independent implementation of the documented one-liner; it also exercises the rejection path | `TestVerifyCodeIsPinnedToTheV1Derivation` |
| The ten other catalog entries still described `DEVICE IDENTITY CONFIRMATION REQUIRED` as a fingerprint-only errand | their shared prose names both forms | `TestContractDocIsInSync`, catalog tests |

## Deliberately left out

- **A code on the *pairing* prompt.** The same mechanism could make the device's
  own approval card show a code derived from a nonce in the controller's hello.
  It would need either a pre-pin hello (rejected, ADR 0015) or a second nonce
  carried by hand, and the identity step is the one that blocks a newcomer.
- **The local TTY front-end still renders no pairing requests**
  (`internal/agent/agent.go` `runConsolePrompt`): an interactive device owner
  sees nothing while an unknown dial blocks for the timeout. Separate defect,
  separate change.
- **A changelog entry.** The repo's practice is a changelog-only PR on top of the
  feature, and the next version number is the release owner's call.
- **`wanctl verify` on the MCP surface.** It is run *on the device*, by a human
  standing at it; a model has nowhere to run it.
