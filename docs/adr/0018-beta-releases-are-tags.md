# 0018 — Beta releases are tags, and the installed version is the channel

Date: 2026-10-20
Status: accepted; amends ADR 0006 ("there is no channel setting")

## Context

ADR 0006 made every device update itself within about six hours of a
publication, which made publishing a release the moment of deployment for the
whole fleet. There was no way to put a change in front of a few volunteers
first. The cost showed: four releases on 2026-09-09 to chase regressions, and
v0.20.0–v0.20.2 shipping an updater that could not update, so every affected
device had to be upgraded by hand to v0.20.3.

The owner proposed three long-lived branches mapped to three artifacts: `main`
to stable releases, `stage` to betas that users opt into, `dev` to each team
member's own test builds.

## Decision

**One long-lived branch; betas and stables are tags on it.** All pull requests
merge to `main`. A beta is a tag `vX.Y.Z-beta.N`. A stable `vX.Y.Z` is the
commit of the last beta that passed promotion, plus at most a commit that only
touches `internal/portal/changelog/`; the release workflow refuses any other
stable tag. A `release/vX.Y` branch is cut from a beta tag only when a beta or
the current stable needs a fix and `main` already holds work that should not
ship with it; fixes land on `main` first and are cherry-picked. Only the newest
stable line is maintained.

**The version grammar gains exactly one pre-release form.**
`vMAJOR.MINOR.PATCH-beta.N`, N a positive integer without leading zeros,
ordered `v0.26.0-beta.1 < v0.26.0-beta.2 < v0.26.0`. Every other pre-release
spelling and all build metadata stay invalid.

**The channel is the installed version, not a setting.**
- A device running a stable (or `dev`) build only accepts a newer stable. A
  beta served where a stable was expected is refused after verification: the
  signature says the bytes are ours, not that they are meant for this device.
- A device running a beta accepts a newer beta or a newer stable. Today its
  update sources only ever offer stables, so in practice it stays on its beta
  until the stable passes it, then rejoins stable for good.
- `wanctl update <version>` installs that exact signed version, including an
  older one. It is how a person joins a beta, and how they leave one at once.

**Betas are GitHub prereleases and nothing else.** Never marked latest, so
`releases/latest/download` — the only source an existing binary reads besides
the relay mirror — never offers one. No APK, no container image, no portal
changelog entry; notes are generated from the merged pull requests.

**The production relay and portal run stable releases only.** Beta clients are
tested against the production stable server. A feature that needs new server
behaviour ships its server half first, compatible and inert, in a stable
release.

**Development builds stay at version `dev`** and are built locally. They are
never offered by any source and never replaced by auto-update.

## Alternatives rejected

- **Long-lived `stage` and `dev` branches.** A tag already says who an artifact
  is for; a second integration branch is one more copy to reconcile. The
  internal fork's `deploy/v0.1.0` diverged from `main` until it could not be
  merged. Promoting by merging `stage` into `main` also produces a commit that
  can carry fixes no beta ever ran.
- **A persisted `channel=beta` setting.** It outlives the beta it was set for:
  a device that rejoined stable would be pulled into the next beta. ADR 0006's
  reasoning about knobs applies unchanged.
- **A beta feed on the relay's `/dl`.** The dist handler answers 503 once its
  directory changes until the relay restarts (`internal/relay/dist.go`), so
  every beta would restart production.
- **Listing releases through the GitHub API.** Rate limits, pagination and a
  second availability dependency, to find something a tester can be told.
- **Deploying the server at beta time.** There is one shared instance; a beta
  server there is a beta for every user, whatever their client runs.
- **Contributor-only dev artifacts.** On a public repository every signed-in
  GitHub user has read access, which is all an Actions artifact requires. Not
  needed either: a `dev` build is never offered to anyone.

## Consequences

- A beta tester runs `wanctl update` again for each new beta. If testers ask
  for betas to follow automatically, or devices that cannot reach GitHub need
  betas, a beta source can be added later without changing the rules above.
- Stable releases come out less often: every one waits for a beta, a soak and
  an update rehearsal (`docs/releasing.md`).
- A stable's own updater has performed a real update before it is published,
  because the rehearsal installs a second beta from the same commit through the
  automatic path.
- The first version carrying this code reaches devices as an ordinary stable.
  Binaries older than it cannot parse a beta and cannot run
  `wanctl update <version>`; they keep updating exactly as before.
- Rolling a device back to an older stable sticks only with `auto_update=off`;
  otherwise the next check reinstalls the newest stable. ADR 0006's "no
  rollback" stands for the fleet.
