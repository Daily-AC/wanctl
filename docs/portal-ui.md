# Portal UI principles

The portal (`internal/portal/index.html`, a single embedded file with no build
step) was rebuilt in September 2026. This page records the rules the rebuild
follows so later changes keep the same shape. It is deliberately short: if a
rule needs a paragraph to justify, it is probably not a rule.

## What the portal is for

wanctl is a security tool: a device only moves when its owner nods. The web
portal exists for the three moments where a terminal is the wrong place to be:

1. **Something is waiting for you** — an approval, a pairing, an identity change.
2. **Naming and sharing** — aliases, friends, grants, tokens.
3. **Onboarding** — install, enroll, read the docs.

Everything on the home page serves the first moment. Everything else is one
click away, ordered by how often it is used.

## Rules

- **Colour carries meaning only.** Green is online or allowed, amber is
  "needs you", red is denied or destructive. No decorative colour, gradients,
  mascots or floating shapes. Identifiers (hostnames, fingerprints, commands)
  are monospace; everything else is the system font.
- **Motion has a job.** A view fades in (120 ms) so a route change reads as a
  change; an approval card fades out when decided; a switch slides. Nothing
  animates on its own. `prefers-reduced-motion` turns it all off.
- **Errors do not vanish.** A failure renders as a persistent notice in the
  page, with the server's message and a retry action, until the user closes
  it or the condition clears. Toasts are for success only, and last two
  seconds. A device that cannot be reached is a *state* of the device page
  ("offline since…", or "online but the console refuses us, here is why"),
  never a transient banner.
- **Frequency decides placement.** Home shows every pending decision across
  the fleet so approvals do not require finding the right device first. The
  sidebar groups pages by how often they are touched: devices daily, access
  weekly, setup once. Mobile gets a four-item tab bar and the rest under
  "更多".
- **Forms say what they need before you fail.** Every field has a label, a
  placeholder and help text; validation is inline; the primary button is a
  verb ("签发", "共享"). No form has a hidden gesture (the old "empty title
  deletes the group" is gone — deleting is a checkbox that says delete).
- **Aliases are names, hostnames are identifiers.** A device shows its alias
  first and its hostname second, everywhere, and the alias is accepted as
  `--target`. The header offers to name a device that has no alias yet.
- **Nothing external at runtime.** No web fonts, no CDN. The CSP allows only
  the page itself plus `api.github.com` for release metadata on the install
  page.

## Vocabulary

The approach has established names, in case they help when reading design
literature: *functional minimalism* (Rams' "less, but better"), *calm
technology* (Weiser and Brown: the interface stays in the periphery until it
matters), *progressive disclosure*, *task-frequency information architecture*
(Hick's law: fewer visible choices, faster decisions), and *purposeful
motion* (animation that explains a state change instead of decorating it).
