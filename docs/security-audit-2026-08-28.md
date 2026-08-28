# Security and architecture audit, 2026-08-28

This review covers the public `wanctl` lineage at v0.3.2 through the audit
follow-up on `main`. It includes the relay data and control planes, portal,
device agent, controller and MCP surfaces, Android elevation, release chain,
CI, repository operations, and architecture redundancy. A prompt-injected
controller holding a valid namespace token and a less-trusted ACL grantee are
both treated as realistic attackers.

The campaign produced 44 ID-bearing security findings and 26 architecture or
operations findings. Duplicate IDs from independently reviewed scopes are kept
below because they provide useful cross-checks (notably SEC-B-01/SEC-C-02 and
SEC-B-06/SEC-C-04).

## Handoff audit corrections

The coordinator independently checked the previous session before continuing.
Four material corrections were required:

1. The 11 security commits did exist on GitHub `main`, but the latest three CI
   runs were red, not green. The Go test job passed; the supply-chain job found
   HIGH CVE-2026-14456 in Alpine's `libcrypto3` and `libssl3` 3.5.7-r0. The
   follow-up image build upgrades runtime packages to 3.5.8-r0; the full local
   CI-equivalent scan then reported zero HIGH/CRITICAL findings.
2. The local findings register contained only SEC-D2 and ARCH-H. The other
   seven reviewer reports remained in session scratch space, so "all findings
   recorded" was not true. This document is the consolidated durable record.
3. SEC-E-01 and SEC-E-02 were only partially fixed. The MCP file tools still
   defaulted to nearly the whole host filesystem, and first-contact device
   identity could still be self-approved by repeating the relay-supplied
   fingerprint. The follow-up confines files to the MCP working directory and
   disables model-callable TOFU pinning by default.
4. Production had only the Android enrollment portal change. The relay was
   still the v0.3.2-era image and the portal used a temporary enrollment commit;
   the remaining security changes had not been deployed at review time.

## Issue triage

| Tracker | Item | Result | Status at audit close |
|---|---|---|---|
| GitHub | #3 Android enrollment dead-end | Root cause was missing relay discovery, not a missing portal input. Added public `/api/instance`, client discovery, persistence, and Android status copy. | Fixed and closed; verified on a real Android device and production portal. |
| GitHub | #4 committed APK/default portal | Premise was false: `build/` is ignored and no APK is tracked. The real UI-legibility concern was fixed with #3. | Closed after the final audit push with the repository/history evidence. |
| Internal | #30 auto-trust has no durable record | Auto-trust and portal pairing admissions now append `trust` events; reconnects do not duplicate them. | Code fixed in public lineage; tracker remains open until lineage/release disposition is explicit. |
| Internal | #28 approval card never arrives | Code path is not the claimed cross-transport defect. The reported 502 is outside the relay handler's possible responses and requires production service-log diagnosis. | Open operational investigation; do not claim code-fixed. |
| Internal | #26 Android built-in verbs | Battery exists. The uncommitted `open <url>` prototype would bypass approval in bypass mode. | Open; do not port `open` until it has a non-bypassable policy class. |
| Internal | #19 long-lived trusted sessions | Product enhancement, not a demonstrated regression. | Roadmap. |

## Security findings

Status vocabulary: **fixed** means the default path is covered by code and a
regression test; **partial** names the remaining boundary; **open** is recorded
work, not an implicit acceptance.

| ID | Sev | Finding | Disposition |
|---|---:|---|---|
| SEC-A-01 | High | One token could create unbounded HTTP device-registry rows and OOM the shared relay. | **Fixed** in `ce1d93d`: per-namespace cap plus stale-entry reaping. |
| SEC-A-02 | Low | HTTP session up/down/close accepted any valid namespace token when the session ID was known. | **Fixed** in `ce1d93d`: sessions bind caller and owner namespaces. |
| SEC-A-03 | Low | Abandoned HTTP sessions and their queues were never reaped. | **Fixed** in `ce1d93d` plus `8cc1824`: idle reaper also starts for HTTP-controller to WebSocket-agent sessions. |
| SEC-A-04 | Low | `/skills` reflected request Host/proto into AI-controller instructions and was publicly cacheable. | **Fixed** in `8cc1824`: `WANCTL_PUBLIC_ORIGIN` is required; unset returns 503. |
| SEC-A-05 | Low | Deprecated `?token=` admission remains accepted, exposing bearer tokens to URL logs. | **Open** server compatibility removal. The last in-tree caller now uses `Authorization: Bearer`. |
| SEC-B-01 | High | A shared-device grantee could read owner-only console, logs, approval events, trust fingerprints, and notify email through the privileged portal. | **Fixed** in `785e818`: all four routes require ownership. |
| SEC-B-02 | High | Unbounded JSON bodies, including unauthenticated enrollment exchange, could inflate relay RSS by gigabytes. | **Fixed** in `f33a385`: route-specific 64 KiB/1 MiB/16 MiB caps. |
| SEC-B-03 | Medium | Control-plane mutations lack a complete actor/object audit trail; dial audit drops caller namespace. | **Open**; requires an audit schema and coverage pass. |
| SEC-B-04 | Medium | A bare namespace token can create durable friendship/share authority without account-level step-up. | **Open** product boundary: introduce account/device token kinds or move mutation behind the portal. |
| SEC-B-05 | Medium | The first successful GitHub login claims administrator with no configured operator binding or role rotation command. | **Open**; bootstrap identity and role-rotation design required. |
| SEC-B-06 | Low | Every enrollment page load minted a never-expiring token before redemption. | **Fixed** in `05430fc`: token issuance occurs only after one-time code exchange. |
| SEC-B-07 | Low | Admin and enrollment failure surfaces have no rate limits; admin secret had no strength floor; upstream negative cache is unbounded. | **Partial**: `8cc1824` enforces a 32-byte admin-secret floor. Rate limiting and miss-cache bounds remain open. |
| SEC-B-08 | Info | Admin ACL creation defaulted omitted permissions to `exec,read,write`, wider than the CLI default. | **Fixed** in `8cc1824`: admin ACL requests must name permissions. |
| SEC-B-09 | Info | GitHub-login invites bind to a mutable handle rather than immutable account ID. | **Open** compatibility/data migration work. |
| SEC-C-01 | High | Device-controlled console/log fields reached SPA `innerHTML`, yielding stored XSS and portal-session control. | **Fixed** in `05430fc`: both sinks escape. CSP still permits the embedded inline script and should be split later. |
| SEC-C-02 | Medium | Shared-device portal reads exceeded ACL capabilities. | **Fixed** with SEC-B-01 in `785e818`. |
| SEC-C-03 | Medium | Encoded traversal in a docs slug made the portal an unauthenticated GET proxy to private relay paths. | **Fixed** in `05430fc`: flat slug allowlist. |
| SEC-C-04 | Medium | Side-effectful GET enrollment created permanent orphan credentials. | **Fixed** with SEC-B-06 in `05430fc`; code minting itself remains a bounded five-minute in-memory operation. |
| SEC-C-05 | Low | A TAB/control-character bypass in OAuth `next` produced an open redirect. | **Fixed** in `05430fc`: all controls and backslash are rejected. |
| SEC-C-06 | Info | Namespace lookup is an enumeration oracle; Lark click identity is not checked for group-chat use; JSON case behavior is fragile. | **Open** defense-in-depth; current one-to-one Lark delivery limits impact. |
| SEC-D1-01 | High | Global/bypass file rules could overwrite the agent's own trust/policy/identity and escalate to console admin. | **Fixed** in `c441106`: config tree and running binary are never transferable. |
| SEC-D1-02 | High | PowerShell subexpressions bypassed POSIX-parsed command-prefix rules. | **Fixed** in `300b7c1`: PowerShell prefix matching rejects evaluation constructs; exact rules remain explicit. |
| SEC-D1-03 | Low | Synchronous exec uses a background context, has no max runtime, and survives controller disconnect. | **Open**; async jobs are bounded, synchronous process lifetime is not. |
| SEC-D1-04 | Info | Pairing name/label is attacker-authored but presented as identification. | **Open** UI copy hardening. |
| SEC-D2-01 | Medium | Internal uncommitted `open <url>` runs as ordinary exec and bypass mode would auto-allow phishing UI. | **Open** outside public tree; assign a non-bypassable policy class before porting. |
| SEC-D2-02 | Low | Android ADB elevation verifies no adbd server certificate; a local app can impersonate a loopback endpoint. | **Open**; add TOFU/SPKI pinning. |
| SEC-D2-03 | Info | Elevated exec shares the ordinary Exec capability, so a grantee can raise an owner approval prompt. | **Open** defense-in-depth capability split. |
| SEC-E-01 | High | MCP push/pull exposed arbitrary MCP-host filesystem read/write. | **Fixed** in `b55f6f0` plus `8cc1824`: HTTP mode disabled; stdio defaults to its working directory, rejects symlink escape, and always excludes wanctl credentials. |
| SEC-E-02 | High | The model could pin a relay-supplied first-seen device fingerprint and collapse E2E TOFU. | **Fixed by default** in `8cc1824`: MCP pinning is disabled; an explicitly named unsafe env opt-in preserves legacy behavior. Shared HTTP MCP therefore fails closed on first contact. |
| SEC-E-03 | Medium | Any remote MCP tenant could read operator server logs with the process's ambient admin secret. | **Fixed** in `b55f6f0`: server logs are stdio-only. |
| SEC-E-04 | Medium | Seven-day MCP rebind bearer is returned to the model; logout revocation disappears on restart. | **Open**; shorten/bind credentials and persist revocation. |
| SEC-E-05 | Low | HTTP MCP lacked browser Origin/Host checks and endpoint authentication. | **Partial** in `b55f6f0`: browser Origins are deny-by-default. Host allowlisting and transport authentication remain open. |
| SEC-E-06 | Low | Hostile device output reaches TTY/model context without control-character or trust-boundary sanitization. | **Open**. |
| SEC-E-07 | Low | Plain `http://`/`ws://` endpoints carry admission/enrollment credentials without warning. | **Open**; LAN compatibility needs an explicit insecure opt-in design. |
| SEC-F-01 | Medium | Releases used Go 1.25.5 while the green vulnerability scan examined 1.26.6. | **Fixed** in `6eb19f4`: go.mod is the single 1.26.6 pin; scan and Docker contract tested. |
| SEC-F-02 | Medium | No branch/tag ruleset, required release reviewer, or immutable releases; docs claimed otherwise. | **Partial**: workflow now uses `--verify-tag` and docs are honest. Repository settings remain an owner decision because a single-maintainer reviewer rule can deadlock releases. |
| SEC-F-03 | Medium | Floating Actions and job-wide signing secrets exposed the release trust root. | **Fixed** in `838bca1`: full action SHAs, step-scoped secrets, no release cache. |
| SEC-F-04 | Low | Release publication did not depend on vulnerability/completeness gates and could omit APKs. | **Fixed** in `838bca1`: scan and shared validator gate publication; APK secrets are mandatory. |
| SEC-F-05 | Low | Android keystore password appeared in argv and decoded keystore survived failed builds. | **Fixed** in `838bca1`: env password source, 0700-era umask, exit trap. |
| SEC-F-06 | Low | Portal/relay recommend bootstrap installers not covered by the signed artifact manifest. | **Open**; prefer GitHub release and add installers to the signed manifest before treating relay bootstrap as strong. |
| SEC-F-07 | Low | Two slow `/dl` readers held both verification slots and denied other downloads. | **Fixed** in `838bca1`: slot released after verification, before streaming. |
| SEC-F-08 | Info | Release documentation named dead paths and pre-OSS behavior. | **Fixed** in follow-up for build, notes, APK, compose, and repository-protection claims. |
| SEC-F-09 | Info | PowerShell installer accepted non-HTTPS and HTTPS-to-HTTP redirects without a TLS floor. | **Fixed** in `838bca1`. |
| SEC-F-10 | Info | CGO-free binaries reproduced bit-for-bit; Android/NDK builds lack an equally pinned reproducibility record. | Recorded; Android toolchain pinning remains future release-engineering work. |

## Architecture and operations findings

| ID | Kind | Finding and disposition |
|---|---|---|
| ARCH-G-01 | parallel paths | WS and HTTP device/session registries plus bridge logic duplicate semantics; **open L-sized relay redesign**. |
| ARCH-G-02 | parallel paths | Three agent uplink loops have divergent reconnect/failure behavior; **open M**. |
| ARCH-G-03 | duplicated logic | Unsafe `strings.Replace(...,"ws","http",1)` rewrote `aws`/`news` hostnames; **fixed in `8cc1824`** with one parsed URL helper. |
| ARCH-G-04 | config sprawl | Portal and remote MCP bypass persisted config; resolver/display helpers are duplicated; **open S/M**. |
| ARCH-G-05 | parallel trust stores | Portal-admin membership is duplicated across two JSON stores with compensating writes; **open M**. |
| ARCH-G-06 | compatibility shim | In-tree MCP rebind kept deprecated query-token auth alive; **partial**: caller now uses Bearer, relay fallback still open. |
| ARCH-G-07 | duplicated clients | Relay/portal HTTP API calls use many clients, status conventions, and timeouts; **open M**. |
| ARCH-G-08 | product branch | LAN fast path adds a third token-store/uplink surface but no public deployment enables it; **owner decision** to isolate or remove. |
| ARCH-G-09 | parallel trust models | Relay behavior changes on `admin == nil`, including permissive legacy docs rules; **open M**. |
| ARCH-G-10 | duplicated dispatch | Device built-ins use three dispatchers/parsers and three POSIX quoting helpers; **open S/M**. |
| ARCH-G-11 | copied wire shapes | Relay/portal/CLI duplicate JSON contracts and error tokens; **open M**. |
| ARCH-G-12 | dead code | Old direct-TCP binding, console approvers, aliases, and compatibility flags are unreachable or expired; **open S**. |
| ARCH-G-13 | duplicated redaction | Device/server/portal secret redaction vocabularies diverge; **open S**. |
| ARCH-G-14 | stale copy | Public MCP/help comments still name internal-era login/deployment behavior; **open S**. |
| ARCH-H-01 | lineage | Public, internal development, and internal deploy histories are hand-maintained copies; **owner decision** to declare one development source. |
| ARCH-H-02 | artifacts | Internal deploy history commits large release tarballs; **owner/internal cleanup decision**. |
| ARCH-H-03 | docs | Two release-note trees disagree; embedded changelog is the active source; **open S cleanup**. |
| ARCH-H-04 | deployment drift | Signing docs described old distribution and compose stamped images `dev`; **partial follow-up fix** adds compose version arg and corrects docs. Operators must set it. |
| ARCH-H-05 | dual source | Portal docs are writable both in git and Postgres; **owner decision** to embed/read-only or make sync a release gate. |
| ARCH-H-06 | config drift | Go toolchain had three pins and two values; **fixed** in `6eb19f4`. |
| ARCH-H-07 | stale refs | Internal repo has merged/gone branches, wrong upstreams, and a dead worktree; **owner/internal cleanup decision**. |
| ARCH-H-08 | stale handoff | Internal handoff links deleted paths and multiple conflicting production versions; **owner/internal cleanup decision**. |
| ARCH-H-09 | docs | Duplicate ADR number 0002 and an orphaned completed plan; **open S**. |
| ARCH-H-10 | release duplication | ldflags/version/notes still have multiple encodings although the platform matrix is single-sourced; **open S**. |
| ARCH-H-11 | public record | GitHub #4 says APKs are committed; history and ignore rules prove none are; **resolved, close issue**. |
| ARCH-H-12 | docs | User-facing build/install variables were missing from the environment reference; **fixed in follow-up**. |

## Verification

The audit follow-up was accepted only after all of the following completed:

- `go test ./... -count=1`
- `go test -tags lark ./... -count=1`
- `go vet ./...` and `go vet -tags lark ./...`
- race tests for relay, MCP, portal, server, and policy
- every release target cross-compiled through `scripts/cross-compile.sh`
- `scripts/security-scan.sh all`: govulncheck, production image build,
  non-root relay/portal/MCP smoke, CycloneDX SBOM, and Trivy image scan
- production deployment checks are recorded separately from code validation;
  a green local test is not evidence that production changed.

## Residual priorities

Before a new public release, the highest-value remaining actions are: configure
GitHub release protections without deadlocking the single-maintainer workflow;
decide how shared HTTP MCP obtains an independently verified first-contact
device pin; bound and persist MCP rebind credentials; and move relay-served
installers under the signed manifest. The larger registry/uplink and lineage
consolidations are architectural projects, not safe drive-by refactors.
