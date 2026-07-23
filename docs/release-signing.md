# Release signing

wanctl releases use an offline Ed25519 key. The relay is an untrusted mirror: it
may serve release files, but neither the updater nor an installer accepts a
binary unless its exact metadata is present in a valid signed manifest.

## Trust bootstrap

Do not fetch an installer from the artifact relay. If the relay is compromised,
an attacker can replace both that script and any public key embedded in it.
Obtain `install.sh` or `install.ps1` from the independently authenticated GitLab
release, or build it from a reviewed Git commit. Keep the installer with the
release notes so its embedded public key is auditable. Both installers require
OpenSSL 1.1.1 or newer and fail closed when Ed25519 verification is unavailable.

The release build injects a comma-separated set of trusted raw Ed25519 public
keys into `internal/release.TrustedPublicKeys`. A normal build has no trust key;
`wanctl update` and relay `/dl/*` distribution therefore remain disabled.

## CI secret

Generate the signing seed on an offline administrative machine:

```sh
openssl rand 32 | base64
```

Store that value as a masked, protected, environment-scoped GitLab CI variable
named `WANCTL_RELEASE_SIGNING_KEY`. Do not store it in the repository, a Docker
build argument, an image layer, the relay filesystem, or a general-purpose app
environment. Restrict the `release` environment and protected version tags to
release maintainers. The release job aborts if the key is absent.

The job builds each platform binary with the matching public key, creates
`manifest.json`, signs its exact bytes as `manifest.json.sig`, and publishes the
release directory as a CI artifact. Deploy that directory read-only at
`WANCTL_DIST_DIR`. The relay verifies the manifest and every file before serving
anything. A bad or incomplete directory returns HTTP 503 for all `/dl/*` paths.
Build the relay image with the public release metadata (public keys are not
secrets), then mount the same signed directory read-only:

```sh
docker build --build-arg WANCTL_VERSION=v1.2.3 \
  --build-arg WANCTL_RELEASE_PUBLIC_KEYS='<current[,overlap] public keys>' .
```

For a local release rehearsal:

```sh
export WANCTL_RELEASE_SIGNING_KEY='<base64 32-byte seed>'
./scripts/build-release.sh v1.2.3
```

## Rotation and revocation

1. Generate a new offline key and protect it as the new CI signing secret.
2. While CI still signs with the old key, set
   `WANCTL_RELEASE_PREVIOUS_PUBLIC_KEYS` to the new public key and publish one
   bridge release. Despite the variable name, this value is an additional trust
   key; inspect the resulting binary and manifest before publication.
3. Wait until the supported fleet has installed the bridge release.
4. Switch `WANCTL_RELEASE_SIGNING_KEY` to the new private key. Set
   `WANCTL_RELEASE_PREVIOUS_PUBLIC_KEYS` to the old public key for one overlap
   release, then remove it from later releases.
5. Redeploy the signed release directory. Never re-sign an existing version;
   publish a strictly greater semantic version.

If the old private key is compromised, skip overlap: remove its public key and
publish a higher version signed by an already trusted recovery key. Devices that
never received that recovery key require an installer obtained through the
independent GitLab/source trust path. Record the revoked key fingerprint and
affected version range in the release notes.

## Failure behavior

The updater rejects unsigned or malformed manifests, unknown fields, an unknown
signing key, bad signatures, missing platforms, same-version replays,
downgrades, artifacts over 64 MiB, size mismatches, and SHA-256 mismatches.
Validation completes in a temporary file before chmod or binary replacement.
`published_at` is signed audit metadata, not a wall-clock gate: device clocks are
not trusted. Monotonic semantic versions provide replay and downgrade defense.
