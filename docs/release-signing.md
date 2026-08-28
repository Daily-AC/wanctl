# Release signing

wanctl releases carry **two signatures over the identical manifest bytes**. The
relay is an untrusted mirror: it may serve release files, but neither the updater
nor an installer accepts a binary unless its exact metadata is present in a valid
signed manifest.

| file | algorithm | verified by | key variable |
|---|---|---|---|
| `manifest.json.sig` | Ed25519 | `wanctl update`, relay `/dl/*` gating | `WANCTL_RELEASE_SIGNING_KEY` |
| `manifest.json.rsa.sig` | RSA-3072 PKCS#1 v1.5, SHA-256 | `install.sh`, `install.ps1` | `WANCTL_RELEASE_RSA_KEY` |

Two algorithms because the two verifiers have different constraints. `wanctl
update` verifies inside Go, where Ed25519 is ideal and has no dependencies. The
install scripts verify in a stock shell, where Ed25519 is not reachable on two of
the three platforms we ship to:

- **macOS** ships LibreSSL as `/usr/bin/openssl`, and its `pkeyutl` has no
  `-rawin`. Ed25519 verification was simply impossible without separately
  installing OpenSSL, so `curl … | sh` failed on any stock Mac.
- **Windows PowerShell** has no Ed25519 at all. Getting it meant installing
  OpenSSL first, a multi-minute download from an origin slow enough that `winget`
  times out on it.

RSA verifies natively in both — `openssl dgst -verify` works on LibreSSL, and
`RSACryptoServiceProvider` is present in the PowerShell 5.1 that every Windows
ships with. Both were verified end to end on real machines before this was
adopted.

Keeping Ed25519 for the updater rather than switching everything to RSA means
already-released binaries keep upgrading normally: a v0.1.2 binary verifies with
the Ed25519 key compiled into it and never looks at the RSA signature.

## Trust bootstrap

The strongest bootstrap is to obtain `install.sh` or `install.ps1` from the
independently authenticated GitHub release, or to build it from a reviewed Git
commit. Keep the installer with the release notes so its embedded public key is
auditable. If the relay is compromised, an attacker can replace both a script
served from it and the public key embedded in that copy — signature verification
cannot save a script the attacker also controls.

The relay serves the installers anyway, because a one-line install is what most
people actually run, and a hardened path they skip protects no one. Treat that
path as "verified against a compromised relay's own key" and prefer the GitHub
release when the machine matters. Verification still fails closed either way: an attacker who can replace
`/dist` binaries but not the served script gets nothing.

Official installers bake the GitHub release base and leave the relay empty.
Self-hosters may instead build them with a default relay for a signed `/dl`
mirror. `WANCTL_DIST_BASE` or `WANCTL_RELAY` overrides either source at install
time.

The release build injects a comma-separated set of trusted raw Ed25519 public
keys into `internal/release.TrustedPublicKeys`. A normal build has no trust key;
`wanctl update` and relay `/dl/*` distribution therefore remain disabled.

## CI secret

Generate both keys on an offline administrative machine:

```sh
# WANCTL_RELEASE_SIGNING_KEY — Ed25519 seed, for wanctl update
openssl rand 32 | base64

# WANCTL_RELEASE_RSA_KEY — for the install scripts. Either DER encoding is
# accepted (genpkey emits PKCS#1 for RSA; other tooling emits PKCS#8).
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -outform DER | base64 | tr -d '\n'
```

Store both as secrets on the `release` GitHub environment, with its deployment
tag policy restricted to release tags. Base64 keeps them single-line, which keeps
secret-masking reliable; the RSA value is ~2.4 KB, comfortably within GitHub's
secret size limit. Losing the RSA key is not fatal to
existing installs — it only signs for new ones — but it does force a public-key
rotation in the next release. Do not store it in the repository, a Docker
build argument, an image layer, the relay filesystem, or a general-purpose app
environment. The repository must additionally enforce protected `v*` tags, a
required environment reviewer with administrator bypass disabled, and immutable
releases. These are repository settings, not properties of the workflow file;
the 2026-08-28 audit found them absent, so release publication remains blocked
on that operator action. The release job aborts if any signing key or Android
keystore input is absent.

The workflow builds each platform binary with the matching public key, creates
`manifest.json`, signs its exact bytes as `manifest.json.sig`, and uploads the
release directory as assets on the GitHub Release. Repository immutable-release
protection must be enabled separately. The directory also contains
`release-public.pem`, which lets a maintainer independently verify the downloaded
release assets without access to the private signing key. Deploy that directory
read-only at
`WANCTL_DIST_DIR`. The relay verifies the manifest and every file before serving
anything. A bad or incomplete directory returns HTTP 503 for all `/dl/*` paths.
Build the relay image with the public release metadata (public keys are not
secrets), then mount the same signed directory read-only. The canonical
self-host path passes these values through `selfhost/.env` and Compose:

```sh
cd selfhost
cp .env.example .env
# Set WANCTL_VERSION and WANCTL_RELEASE_PUBLIC_KEYS in .env, then:
docker compose build
```

For a local release rehearsal:

```sh
export WANCTL_RELEASE_SIGNING_KEY='<base64 32-byte seed>'
./scripts/build-release.sh v1.2.3
go run ./cmd/release-manifest verify release release/release-public.pem
```

Pushing a protected `vMAJOR.MINOR.PATCH` tag starts `.github/workflows/release.yml`.
The workflow requires both signing secrets plus the Android keystore and
password. It publishes all four per-ABI APKs; a release without them is refused
because it would strand installed Android devices.

To publish the same release manually, build or download the complete `release/`
directory, check out that exact tag, authenticate `gh`, then run:

```sh
./scripts/publish-release.sh v1.2.3 release internal/portal/changelog/v1.2.3.md
```

The publisher rejects an existing release, a tag/source mismatch, unexpected or
missing files, a manifest/tag mismatch, a bad signature or artifact hash, and
installers whose embedded key differs from `release-public-rsa.pem` — the Unix
installer carries it as PEM, the Windows one as .NET XML.

Publishing needs neither private key: both signatures are checked against the
public keys shipped in the release directory. Pin `release-public.pem` against
the previous release (or a copy kept outside the artifact) — an artifact that
supplies both key and signature only proves internal consistency.

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
independent GitHub/source trust path. Record the revoked key fingerprint and
affected version range in the release notes.

## Failure behavior

The updater rejects unsigned or malformed manifests, unknown fields, an unknown
signing key, bad signatures, missing platforms, same-version replays,
downgrades, artifacts over 64 MiB, size mismatches, and SHA-256 mismatches.
Validation completes in a temporary file before chmod or binary replacement.
`published_at` is signed audit metadata, not a wall-clock gate: device clocks are
not trusted. Monotonic semantic versions provide replay and downgrade defense.
