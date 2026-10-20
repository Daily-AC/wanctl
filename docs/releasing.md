# Releasing

How code on `main` becomes a beta, how a beta becomes a stable release, and
what each kind of build is allowed to reach. The reasoning is in
[ADR 0018](adr/0018-beta-releases-are-tags.md); automatic updates are
[ADR 0006](adr/0006-cli-auto-update.md).

## Three kinds of build

| Build | Made from | Version | Who gets it | Auto-update |
|---|---|---|---|---|
| Stable | tag `vX.Y.Z` | `v0.26.0` | everyone: GitHub latest, the relay's `/dl`, the installers, the Android app | every stable device follows it within about six hours |
| Beta | tag `vX.Y.Z-beta.N`, a GitHub prerelease | `v0.26.0-beta.2` | whoever runs `wanctl update v0.26.0-beta.2` | never offered to a stable device |
| Development | `go build`, `scripts/cross-compile.sh` | `dev` | whoever built it | never replaced |

A device's channel is the version it runs. On a stable it only takes newer
stables. On a beta it takes a newer beta or a newer stable; since the default
update sources only offer stables, it stays on its beta until the stable passes
it and then is back on stable.

## Branches

`main` is the only long-lived branch. Every pull request merges there, and both
betas and stables are tags.

Cut `release/vX.Y` from the latest beta tag only when a beta (or the current
stable) needs a fix and `main` already holds work that should not ship with it.
Land the fix on `main` first, then `git cherry-pick -x` it onto the branch and
tag the next beta there. Only the newest stable line gets fixes; delete the
branch once the next minor's stable is out.

## Cutting a beta

1. Pick the commit (normally `main`'s head; a `release/vX.Y` head if one exists)
   and make sure CI is green on it.
2. `git tag -a vX.Y.Z-beta.N -m "vX.Y.Z-beta.N" <commit> && git push origin vX.Y.Z-beta.N`.
   N starts at 1 for each X.Y.Z. Tags are never moved or reused.
3. The Release workflow publishes a prerelease with generated notes and the
   signed desktop binaries. It builds no Android artifacts and no container
   image, and needs no changelog file.
4. Install it on the maintainers' own devices:
   `wanctl update vX.Y.Z-beta.N`. To leave a broken beta at once, install the
   current stable the same way. A device that cannot reach GitHub needs
   `HTTPS_PROXY` for this command; the relay mirror only carries the stable.

The production relay and portal are not deployed from a beta.

## Promoting a beta to stable

A stable tag must point at the commit of a `vX.Y.Z-beta.N` tag, or at a commit
that differs from it only under `internal/portal/changelog/`. The Release
workflow refuses anything else. The simplest way to satisfy it is to add
`internal/portal/changelog/vX.Y.Z.md` before the rehearsal beta below, so the
stable tag lands on that beta's commit.

Promote only when all of the following hold:

1. **Soak.** The candidate beta has run on the maintainers' devices for 24
   hours with no open regression.
2. **It starts everywhere.** Installed with `wanctl update <beta>`, the
   candidate comes back up and reconnects under every supervisor: macOS
   launchd, Linux systemd, a Windows device installed with
   `wanctl service install`, and a device with an approval phone attached.
3. **Its own updater works.** Tag a rehearsal beta `vX.Y.Z-beta.(N+1)` from the
   same commit. On each device from step 2, without rebooting:
   ```sh
   # Example: candidate v0.26.0-beta.1, rehearsal v0.26.0-beta.2
   wanctl config set release_base=https://github.com/Daily-AC/wanctl/releases/download/v0.26.0-beta.2
   # restart the agent through its supervisor; the first check runs 60 s later
   # wait for that check to install the rehearsal and restart the agent
   wanctl version            # must print v0.26.0-beta.2
   wanctl config unset release_base
   ```
   Ensure `auto_update` is on and no `WANCTL_RELEASE_BASE` environment override
   masks the persisted setting. The agent must be idle and able to write its
   binary directory for the check to install.

   This is the automatic path, run by the candidate's code. v0.20.0–v0.20.2
   shipped an updater that could not update; this step is what catches that.

An emergency fix for a stable that is hurting users may skip the 24-hour soak.
It may not skip steps 2 and 3: a broken updater is the one fault the next
release cannot repair.

Then:

1. `git tag -a vX.Y.Z -m "vX.Y.Z" <commit> && git push origin vX.Y.Z`.
2. The Release workflow publishes the stable, marked latest, with the APKs, the
   signed container image and `internal/portal/changelog/vX.Y.Z.md` as notes.
3. Deploy the relay and portal from that tag.
4. The stable binaries are rebuilt with the new version string, so check one
   device: it reports `vX.Y.Z` after updating, and a device that only reaches
   the relay's `/dl` mirror picks it up too.

## Server changes

There is one production relay and portal, shared by every user, and it runs
stable releases only. A beta client is always tested against it. So:

- A feature that needs new server behaviour ships its server half first, in a
  stable release, compatible with existing clients and inert until a new client
  uses it. The client half then goes through a beta.
- A migration must leave the previous stable's image able to run on the new
  schema. When it cannot (migration 007 renamed a column), the changelog says
  that rolling back means restoring the database, and the migration is run
  against a copy of the production database before the stable is deployed.

## Development builds

Build locally; nothing is published. The version stays `dev`, so no update
source ever offers the build to anyone and auto-update never replaces it.
`wanctl update` on a `dev` build installs the current stable.
