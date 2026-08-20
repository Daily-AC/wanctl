# ADR 0002 — Device-identity trust must be acknowledgeable, and the portal identity rides on enrollment

Status: accepted (2026-08-06)

## Context

On 2026-08-06 `alice/DESKTOP-AB12CD3` was reinstalled. It reported **online**
in the portal device list and stayed unreachable from the portal for hours. No
action available in any UI could recover it. The cause was three mechanisms that
are individually defensible and jointly a dead end:

1. **The portal's pin.** `deviceConnFor` pinned each device fingerprint with
   `PinServer(..., replace=false)`, seeded TOFU-style from the relay's own
   `/admin/devices` record. Because the seed and the later comparison come from
   the same party the pin is supposed to protect against, it is a *change alarm*,
   not an authentication.
2. **The alarm had no acknowledge path.** A mismatch surfaced as a bare 502 whose
   body the SPA discards. Neither fingerprint was ever shown to the only person
   who could decide about them, and unbinding the device deleted only the
   relay-side record — `known_servers.json` sits on the portal's persistent
   `portaldata` volume and kept the stale pin across unbinds, reinstalls and
   redeploys.
3. **The console-admin set was never seeded.** The agent fails closed on console
   sessions unless the portal's fingerprint is in its `portal_admins.json`
   (`agent.go:375`), and nothing wrote that file: not the installer, not enroll,
   only a hand-passed `--portal-fps`.

The device's identity (`cert.pem`), its token, its trusted controllers and its
portal-admin set all live in one config directory, so "clear the credentials and
reinstall" — the obvious operator reflex, and the one that had just been
performed — resets all four at once and trips every mechanism above
simultaneously. Pairing could not rescue it either: `console.AskPair` denies when
no portal console is attached, and the portal console was the thing locked out.

## Decision

**An identity alarm is only worth raising if a human can clear it.** Three
changes, none of which weaken what the pin actually detects:

- Unbinding a device forgets its pin. Unbinding is the owner stating, over SSO,
  that this name no longer refers to that machine — the out-of-band confirmation
  TOFU requires. Removal is scoped by name (`Store.RemoveName`), because one
  machine is routinely pinned under several names.
- A mismatch returns `409` with both fingerprints, and the portal renders an
  approval card. Accepting is owner-only and must echo the fingerprint the
  browser displayed, so a stale tab cannot turn one confirmation into a standing
  permission.
- The portal declares its own fingerprint when minting an enrollment code; the
  relay carries it on the code; the device trusts it on completing enrollment.

## Trade-off accepted

The portal fingerprint reaches the device **through the relay**. A compromised
relay could therefore nominate itself as console administrator on devices
enrolling from that moment on — a capability it does not have today, when the
value must be passed by hand.

We accept this because the alternative was not "operators pass `--portal-fps`";
it was "nobody passes it, the portal console silently does not work, and the
failure is indistinguishable from the portal being broken". A control that is
never exercised is not a control. The exposure is bounded deliberately:

- seeded **only at enrollment** — an already-enrolled device never gains a
  console administrator from the wire;
- printed by the terminal and shown on the enroll page, which is the one leg of
  the handoff the relay is not on, so substitution is detectable by comparison
  rather than assumed away;
- `--portal-fps` / `WANCTL_PORTAL_FPS` still take precedence and still work for
  anyone who wants the stronger chain.

## Rejected alternatives

- **Let re-enrollment silently re-pin the device fingerprint.** This is the
  intuitive fix ("re-binding should just match the new fingerprint") and it
  destroys the alarm: the party being watched would be able to clear its own
  warning. Only an SSO-authenticated human action clears a pin.
- **Have the device fetch the portal fingerprint from the portal directly.** The
  cleanest trust chain — TLS to the portal domain, no relay involved — but the
  portal is SSO-gated at the platform edge, which is exactly why `/skills`
  already 302s to the relay. There is no unauthenticated portal endpoint to
  fetch from.
- **Make the relay authoritative for device identity.** Removes the pin entirely
  and with it the only detection we have for a relay that starts lying about
  which machine a name refers to.

## Consequences

- Devices enrolled before this change still need a one-time
  `wanctl portal-admins add <portal-fp>`; nothing back-fills them.
- Deployment already stranded one device. Recovery is the new accept card, or an
  unbind followed by re-running `wanctl`.
- If a device is ever genuinely impersonated, the operator sees two fingerprints
  and an accept button. Whether that is a real defence now depends on people
  actually comparing them, which is why the enroll page shows the value the
  terminal will print.
