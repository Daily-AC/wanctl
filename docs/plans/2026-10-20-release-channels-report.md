# Beta releases, phase 1 — report

Spec: [ADR 0018](../adr/0018-beta-releases-are-tags.md) and
[releasing.md](../releasing.md). Branch `feat/release-channels`.

## What changed

- `internal/release`: versions are `vX.Y.Z` or `vX.Y.Z-beta.N`; a stable or
  `dev` build refuses an offered beta with `ErrBetaNotAllowed`, which moves
  the source walk on instead of ending it; a beta build takes a newer beta or
  stable. `SelectExact` serves explicit requests.
- `wanctl update <version>`: exact signed install, downgrades included with a
  warning; GitHub `latest/download` is rewritten to `download/<version>`, then
  a custom base, then the relay mirror; the sudo split passes the version on.
- Scripts: `build-release.sh` builds desktop-only betas; `validate-release.sh`
  drops the changelog and APK requirements for betas and rejects an APK in
  one; new `check-promotion.sh` refuses a stable that is not a beta's commit
  plus changelog-only changes.
- `release.yml`: full history checkout, promotion check before any secret,
  keystore/APK/image only for stables, betas published as non-latest
  prereleases with generated notes, stables explicitly `--latest`.

## Acceptance

Unit and script tests are listed in the implementation commits. Independently,
a harness built before the code was read ran real processes on macOS: a local
relay with its `/dl` mirror, a static server laid out like GitHub releases
(`releases/latest/download`, `releases/download/<tag>`), and releases signed
with throwaway keys. Agents ran under `wanctl agent`; each automatic check was
the real 60-second first check.

| # | Scenario | Result |
|---|---|---|
| S1 | stable device, beta mistakenly at latest, `wanctl update` | stays stable |
| S2 | same, automatic update | stays stable |
| S3 | binary built from pre-change `main`, beta at latest | does not take it; installs the mirror's stable |
| S4 | `wanctl update v0.91.0-beta.1` from a stable | installs the beta |
| S5 | beta agent, latest stable older | stays on the beta |
| S6 | rehearsal: `release_base` at the next beta, restart agent | its own updater installs the next beta and logs it |
| S7 | stable passes the beta | agent rejoins stable |
| S8 | rejoined device, beta at latest | stays stable |
| S9 | explicit downgrade | warns, installs |
| S10 | requesting the running version | no-op, exit 0, binary untouched |
| S11 | version nobody serves | exit 1, each source's reason listed once |
| S12 | `-rc.1` | rejected before any download |
| S13 | explicit install, unreachable base | falls back to the relay mirror |
| S14 | `build-release.sh v0.91.0-beta.3`, no Android environment | signed desktop-only release; validation passes; a stray APK fails it |
| S15 | `check-promotion.sh` in a real clone | same commit and changelog-only pass; code diff, missing beta and beta.9-vs-beta.10 ordering behave |

All 28 checks passed. `gofmt`, `go vet`, `go test ./...`, the `lark` tag, and
Windows and Android cross builds pass.

## Not verified yet

- The Release workflow itself on GitHub: the first `-beta.1` tag after merge is
  its first real run (prerelease flag, `--latest=false`, skipped image/APK).
- Real supervisors other than a bare `wanctl agent` on macOS: launchd, systemd,
  a `service install` Windows device, a device with an approval phone. These
  are promotion step 2–3 in `releasing.md` and belong to the first beta cycle.

## First rollout

Devices on v0.25.0 and older cannot run `wanctl update <version>`, and the next
stable needs a beta first. So for the first cycle only, install `-beta.1` on
the maintainers' devices from the prerelease's assets by hand (verify against
its `manifest.json`); from that beta on, the rehearsal and every later beta use
the command.

## First cycle (2026-10-20)

- `v0.26.0-beta.1` and `v0.26.0-beta.2` (rehearsal, same commit) went through
  the Release workflow: prereleases, 26 assets, no Android artifact, image steps
  skipped, `latest` still v0.25.0. Their real run closes the first "not
  verified" item above.
- beta.1 was installed by hand, checked against the signed manifest whose keys
  match v0.25.0's byte for byte, on two maintainer devices: macOS under launchd
  (approval phone attached) and Windows under a SYSTEM supervisor loop.
- Rehearsal: both agents' own updaters installed beta.2. The Windows one
  needed a second restart: its first check left no log line, probably a
  transient GitHub failure masked by the relay mirror (issue #170).
- Still open: no Linux systemd device was available; the 24-hour soak ends
  2026-10-21 12:45; then tag `v0.26.0` on the beta.2 commit with its changelog.
