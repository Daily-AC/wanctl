# Security issue audit, 2026-07-23

This audit covers every issue that was open in GitLab at the start of the
review (`#3` through `#19`). Each security claim was checked against the code
and, where practical, reproduced with an executable regression test before the
fix was written.

## Executive result

- `#3`, `#4`, and `#5` were directly reproducible critical vulnerabilities.
  They respectively allowed a paired controller to become a persistent console
  administrator, allowed command-policy suffix injection, and allowed command
  injection through `cwd`.
- `#8` had a fleet-wide critical blast radius after compromise of the relay or
  release path, but was not a direct unauthenticated compromise by itself.
- `#6`, `#7`, and `#9` through `#18` contained real security defects, with the
  qualifications recorded below.
- `#19` was not a defect in the current authorization flow. It is a valid
  product enhancement for device-bound refresh sessions and step-up auth.

## Findings

| Issue | Verdict | Severity | Reproduction or qualification | Disposition |
| --- | --- | --- | --- | --- |
| #3 | Confirmed | Critical | A normally paired controller sent `console_hello`, then persisted `bypass`. | Console sessions now require both a relay-issued console capability and membership in the device's portal-admin trust set. |
| #4 | Confirmed | Critical | A rule for `git status` matched `git status && <payload>`. | Command prefixes are parsed as shell AST; compound syntax, redirection, and substitution require an exact rule. |
| #5 | Confirmed | Critical | A hostile `cwd` broke out of the quoted `cd` string while the approved command remained benign. | One-shot jobs use `exec.Cmd.Dir`; persistent shells receive cwd through a private data file instead of shell interpolation. |
| #6 | Confirmed | High | A symlink below an allowed root reached a file outside that root. | File access uses root-scoped opens, rejects link traversal and non-regular files, and carries the matched policy root to the server. |
| #7 | Confirmed | High | Two SSO identities that normalized to one namespace caused the latter identity to replace the former. Header forgery additionally depends on an incorrectly exposed portal backend. | Namespace conflicts fail instead of changing the bound immutable identity. Deployment requirements for the SSO header are explicit. |
| #8 | Confirmed, conditional | High | The updater and installers executed bytes supplied by the relay without an independent trust root. Compromise of the distribution path therefore became fleet code execution. | An offline Ed25519 manifest binds version, platform, name, size, and SHA-256. Updater and relay fail closed without compiled trust keys; relay-hosted installers are retired in favor of independently obtained, key-embedded release installers. |
| #9 | Confirmed | High | Long-lived admission tokens appeared in WebSocket and HTTP query strings. The plaintext LAN transport remains safe only inside the documented encrypted overlay. | New clients use `Authorization: Bearer`; the relay temporarily accepts legacy query clients with deprecation headers for rollout. |
| #10 | Confirmed | High | MCP rebind was recoverable Base64 containing the raw token and a caller-supplied namespace. | Rebind is an AEAD-sealed, audience-bound, expiring claim; restore resolves the token namespace at the relay. Logout revocation is currently process-local. |
| #11 | Confirmed | High | An ACL row's `perms` value was displayed but ignored by the tunnel, so a read or exec grant opened the full protocol. | The relay issues authenticated session capabilities and the device enforces them for exec, read, write, logs, and console. |
| #12 | Confirmed, scoped | High | Relay bodies and background jobs had practical unbounded memory/concurrency paths and HTTP servers lacked complete timeouts. The wire frame already had a 16 MiB cap and MCP sessions already expired. | Relay bodies, HTTP timeouts, job concurrency, runtime, output, count, bytes, and retention are bounded. Aggregate session queues and synchronous exec lifetime remain separate hardening work. |
| #13 | Confirmed | Medium | Event logs retained raw command/path secrets indefinitely and log reads had no separate capability. | Common credentials are redacted, logs rotate within a quota, and log reads require a distinct capability. Free-form positional secrets can still evade heuristic redaction. |
| #14 | Confirmed, conditional | Medium | Cross-site mutation defenses, framing policy, CSP, body limits, URL sanitization, and production-safe `/whoami` behavior were absent. Exploitability of CSRF depended on the surrounding SSO cookie/proxy policy. | Mutations require same-origin double-submit CSRF, responses carry browser security headers, Markdown allows only HTTP(S) or safe relative URLs, and `/whoami` is disabled by default. |
| #15 | Confirmed | Medium | Upload opened the destination with `O_TRUNC` before the stream completed and accepted unbounded/mismatched sizes. | Uploads are size-bounded, exact-length checked, written to a same-directory temporary regular file, synced, and atomically renamed. |
| #16 | Confirmed | Medium | Pins were indexed only by device name, so same-named devices in different namespaces collided. First contact was also silently trusted. | Pins use canonical `owner/device`; first contact and certificate changes require explicit out-of-band confirmation or authenticated portal pre-pinning. |
| #17 | Confirmed, architectural | Medium | A production portal fingerprint compiled into every binary acted as a non-rotatable fleet root. | Self-hosted binaries have no company portal root; devices support versioned multi-root overlap, explicit rotation, revocation, and legacy migration. Existing sessions are not forcibly disconnected after revocation. |
| #18 | Confirmed, partial | Medium | [Alpine's release table](https://www.alpinelinux.org/releases/) lists 3.20 support ending on 2026-04-01; image tags were mutable, containers ran as root, and installer URLs trusted request Host/proto. Internal deployment documentation was operational metadata, not itself a code vulnerability. | Images and CI tooling are digest-pinned, runtime is non-root, request-derived installers are retired, and CI adds vulnerability, SBOM, and image scanning. |
| #19 | Not a bug | Enhancement | The CLI already stores a long-lived admission token, and HTTP MCP already had a rebind path. The issue asks for a safer, more complete refresh-token and step-up-auth product model rather than describing a current repeated-code defect. | Keep as a roadmap enhancement; do not close it as a security fix. |

## Residual boundaries

The fixes do not make a plaintext LAN relay confidential; deployments must use
an encrypted overlay or `wss://`. Token rotation, process-independent MCP rebind
revocation, forced termination of already-established portal sessions, and the
larger device-bound refresh-session design from `#19` remain explicit follow-up
work rather than hidden claims of completion.

The repository now fails closed when a release trust key or signed directory is
missing, but no production signing seed was generated during this audit. An
administrator must configure `WANCTL_RELEASE_SIGNING_KEY` as a masked,
protected, release-environment-scoped GitLab variable before the first release.
The project also had no GitLab runner and the local Docker daemon and PowerShell
runtime were unavailable, so container build/SBOM/Trivy smoke and the PowerShell
installer still require execution in the configured release environment.
