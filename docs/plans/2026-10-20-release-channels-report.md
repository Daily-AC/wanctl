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
- Linux systemd: a transient system unit on a spare VM (no relay, no
  enrollment) ran beta.1, which updated itself to beta.2 in place: same
  MainPID, zero restarts. The unit was removed afterwards.
- A Windows device under `wanctl service install`'s own `__supervise` task
  (2026-10-21, a test VM restored from a snapshot with v0.24.0 installed that
  way): a manual swap to beta.2 was restarted by the supervisor;
  `wanctl update v0.26.0-beta.1` replaced the binary and restarted the agent
  (exit 0) while the supervisor itself kept running from a renamed old image;
  with `release_base` at beta.2 the agent's own check installed it 70 s after
  start. The VM reaches GitHub only through a proxy, so `HTTPS_PROXY` was set
  for the command and, for the rehearsal, in the user's environment before
  restarting the task.

## Promotion (2026-10-21)

- The soak ended at 12:45 with both maintainer devices online and no open
  regression. Minutes earlier govulncheck began failing on GO-2026-6809
  (crypto/x509, fixed in Go 1.26.10), so the Release workflow would refuse to
  build v0.26.0 on the beta.2 commit.
- PR #177 added the changelog and Go 1.26.10. `v0.26.0-beta.3` and the
  rehearsal `v0.26.0-beta.4` were tagged on its merge commit, and steps 2 and
  3 were rerun under all four supervisors (launchd with the approval phone,
  the SYSTEM loop, `service install`, a systemd user unit). Every one installed
  beta.3 with `wanctl update` and beta.4 with its own updater.
- The second 24-hour soak was skipped under the emergency-fix clause: the code
  was the soaked beta.2 code plus the toolchain patch, and the stable it
  replaced was built with the affected Go. `v0.26.0` was tagged on the same
  commit (`check-promotion`: promotes beta.4) and deployed.
- After the deploy, the systemd unit rejoined the stable 61 s after a restart,
  and the `service install` VM, which reaches only the relay mirror, picked it
  up from `/dl` about 3 minutes after a restart.
