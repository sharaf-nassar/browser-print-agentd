# Infrastructure

The repository-side gates: what makes this tree safe to publish, and what any path that ships a
`.pkg` is held to.

## Naming Gate

`scripts/check-naming.sh` is "safe to publish" as one command with one exit code. This tree was
extracted from a private monorepo, and the gate is what keeps the scrub a checked claim rather
than a remembered one.

It asserts four classes. **Class 1** bans the originating org and project names. **Class 2** bans
hardware and lab artifacts — a printer serial, an Apple Team ID, a station IP, lab queue names.
Neither class has an exception mechanism: this repository is independent of the monorepo it was
extracted from, so there is no legitimate reason for any of those strings to appear in it, and a
hit is always a failure. **Class 3** is a YAML shape check on the release workflow's trigger
surface, so a temporary dry-run trigger cannot survive into the default branch. **Class 4** is
identity consistency: `packaging/identity.sh`'s `BINARY_NAME`
against `[[identity.go#productName]]`, the `go.mod` module path, and the frozen
`X-Print-Agent-Version` header.

Two design choices carry most of the weight. Searching uses `git grep` over tracked files rather
than `grep -ri`, so build artifacts and untracked scratch files can neither produce a phantom hit
nor bury a real one under noise. And the banned patterns are written with their first character
bracketed (`[n]asha` matches the text but is not the text), which means the gate file spells none
of the strings it bans and can therefore be scanned by itself — an excluded gate file would be a
blind spot in exactly the file a careless paste would most like to use.

### Zero Exceptions

Class 1 and class 2 are absolute: there is no allowlist file, no marker token, and no per-file
budget that can excuse a hit. A red gate is fixed by renaming the string, never by widening the
gate.

The mechanism was deliberately removed rather than left empty. This repository once carried three
declared exceptions — an old-label cleanup path in the installer, a provenance line in the
`README.md`, and a vendored manifest pinning the originating repository — and all three came out
with the subsystems that needed them. Keeping an exception mechanism with nothing in it invites
the next hit to be declared rather than fixed, so the gate now treats class 1 exactly like class
2 and reports the file, the line, and the matched token on any hit at all.

## Release Chain

What any path that ships a `.pkg` is held to: gate, sign, notarize, staple, verify the packaged
binary, then publish an asset that is never overwritten.

The repository side of that contract is `packaging/build-pkg.sh`, `packaging/identity.sh`, and
the `-X main.version` linker seam; a CI workflow drives it but adds no requirement of its own.

The payload is not built by the pipeline. A release imports the Developer ID identities and hands
them, with the version resolved from the tag, to
[[packaging#Packaging#Packaging Identity#Build Script Environment Interface|`build-pkg.sh`]],
which owns the binary,
the LaunchAgent, the pre/postinstall scripts and the distribution definition. A release and a
local unsigned build therefore differ only in the two identity flags, so the recipe that ships is
the recipe that gets exercised.

The tag is also the version, linked into the binary through `GO_LDFLAGS` as `-X main.version` and
read back off the built artifact before anything is published: the `.pkg` is expanded with
`pkgutil --expand-full`, the payload's code signature is verified, **that** binary is booted, and
the version is read from both `GET /health` and the `X-Print-Agent-Version` header. A version
that exists only in a linker flag is not evidence, and neither is one reported by a binary that
is not the one inside the package.

### Signing Chain Facts

The chain was proven end to end on a throwaway hello-world daemon before any release pipeline was
written, and two facts it pinned down are load-bearing rather than incidental.

`APPLE_CERTIFICATE` is a single PKCS#12 carrying **both** the Developer ID Application and the
Developer ID Installer identity, so one `security import` covers signing the binary and signing
the package — but the import and `set-key-partition-list` must name `/usr/bin/productsign` and
`productsign:`, which a `.dmg`-shaped recipe does not, and without them `.pkg` signing fails on a
locked key. The two identities are then selected by two different `find-identity` queries,
because the Installer identity never appears under `-p codesigning`. `KEYCHAIN_PASSWORD` is not
provisioned, so the build keychain falls back to an ephemeral `uuidgen` value.

`notarytool submit --wait` runs against the **`.pkg` itself** rather than a zipped binary, and
the Gatekeeper assessment (`spctl --assess --type install`) is taken with the
`com.apple.quarantine` attribute deliberately written on. Nothing removes the quarantine bit
before the verdict, because an install that only succeeds after `xattr -d` proves nothing about
Gatekeeper; the attribute is cleared only afterwards, so the published asset carries none of the
runner's metadata.

### Asset Retention

The asset is named `browser-print-agentd-<version>.pkg`, so every release keeps its own installer
and no prior notarized `.pkg` is ever overwritten. That is the retention half of the documented
downgrade path.

The exact name is asserted before upload rather than trusted to convention, because an
unversioned asset would make a downloaded installer ambiguous about which build it carries. One
version string drives the linker stamp, the package version `build-pkg.sh` hands `pkgbuild`, and
that asset name, so the release path can predict the package path and hard-fail when the script
writes something else instead of discovering whatever it happened to produce.

Retention is only real if a run proves it, so the run that ships a build also names the build a
station falls back to: after upload the previous `v*` release is read back and its installer and
download URL recorded as this release's rollback target. Nothing in the path deletes, and a
`--clobber` on the upload can only ever replace the asset of the tag being released, since a
different tag is a different release. A prior release with no `.pkg` warns rather than fails —
the package just shipped is already published and notarized, and failing there would report a bad
release for a defect in an older one.
