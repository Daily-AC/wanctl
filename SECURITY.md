# Security Policy

wanctl executes commands on remote machines. Vulnerabilities in it can turn
into remote code execution on someone's device, so reports are taken
seriously and handled privately.

## Reporting a vulnerability

Please use GitHub's private vulnerability reporting:
https://github.com/Daily-AC/wanctl/security/advisories/new

Do not open a public issue for anything you believe is a security problem —
that includes policy bypasses, approval bypasses, trust-store confusion,
relay authentication flaws, and anything that lets a controller do more on a
device than its capabilities allow.

You can expect an acknowledgment within a few days. Please include enough
detail to reproduce: component (relay / portal / agent / CLI / Android app),
version (`wanctl --help` prints it; releases are tagged), and the smallest
setup that shows the problem.

## Supported versions

Only the latest release receives fixes. There is no backporting; upgrading is
a single-binary swap (`wanctl update`) on every platform except Android,
where the app updates through its own check-for-update flow.

## Scope notes

- The relay is designed to be untrusted: it sees only ciphertext for
  controller↔device sessions. A report that the relay can read session
  plaintext would be a critical finding.
- The device-side policy engine is the last line of defense. Anything that
  executes without a matching rule or an approval is a finding, including in
  bypass mode's documented boundaries.
- Denial of service against your own self-hosted relay is out of scope.
