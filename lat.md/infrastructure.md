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
surface: the only supported dry run is `workflow_dispatch` on a branch ref, and class 3 reds any
branch that instead adds a `push: branches:` trigger before it can reach the default branch.
**Class 4** is
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

## Continuous Integration

`.github/workflows/ci.yml` runs four gates on every push to `main` and every pull request: the
darwin/arm64 cross-build, the Go unit suite, the [[infrastructure#Naming Gate|naming gate]], and
the `lat check` link check.

All four run on ubuntu with no Apple hardware, no signing identity and no printer. That is not a
concession — cross-compiling the ship target is what keeps the whole build Mac-less, and the unit
suite reaches CUPS only through the stubbed exec runner, so a linux runner can prove both the
binary the `.pkg` will carry and the behavior it will have. `go vet` runs under
`GOOS=darwin GOARCH=arm64` for the same reason: the target that ships is the target worth vetting.

The Go toolchain is read from `go.mod` through `go-version-file` rather than restated in the
workflow, so the pin lives in one place. Module caching is off: this is a stdlib-only module with
no `go.sum`, and enabling the cache would demand a lockfile that never exists.

### Gate-Ran Outputs

The workflow is callable (`workflow_call`) and re-exports one output per job, because a **skipped**
job reports success and a caller otherwise cannot tell "the gate passed" from "the gate never ran".

Each job's final step writes `ran=true`, so the value is `true` only when that job reached the end
of its steps and an empty string when it was skipped or failed. The release workflow calls this
one, so the thing gating a shipped `.pkg` is byte-identical to the thing gating a pull request,
and it refuses to publish on anything but four literal `true`s. Without that read-back a release
could ship on the strength of a job that did nothing, which is the failure mode the outputs exist
to make impossible.

The output ids are snake_case and the hyphenated job ids are dereferenced with index syntax
(`jobs['cross-build']`) rather than as dotted properties, so both halves of the contract stay
readable from a calling workflow.

## Release Chain

What any path that ships a `.pkg` is held to: gate, sign, notarize, staple, verify the packaged
binary, then publish the installer and its metadata assets without overwriting another release.

`.github/workflows/release.yml` is that path, and the repository side of the contract it drives is
`packaging/build-pkg.sh`, `packaging/identity.sh`, and the `-X main.version` linker seam. The
workflow adds no packaging requirement of its own — it imports credentials, resolves a version,
and checks the result.

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

### Trigger Surface

A `vX.Y.Z` tag is the only thing that ships. The published `on:` block carries exactly
`push: tags:` and `workflow_dispatch`, and the concurrency group is `release-${{ github.ref }}`
with cancellation off.

Cancellation is off deliberately: a cancelled release can leave a notarization submission in
flight and a half-attached asset. A stuck release is recoverable; a partially published one is
the thing an operator cannot reason about.

The workflow names no product string of its own. It sources `packaging/identity.sh` for
`BINARY_NAME` and `RELEASE_TAG_PREFIX` rather than restating them in `env:`, so the predicted
`.pkg` path and the [[infrastructure#Release Chain#Asset Retention|asset-name assertion]] derive
from the same value the packaging templates render from. The trigger glob itself cannot be
templated, so the resolved tag is checked against `RELEASE_TAG_PREFIX` at run time — a rename
that missed the `on:` block fails the release instead of cutting a mis-versioned one.

Dry runs are what `workflow_dispatch --ref <branch>` is for: dispatching on a non-tag ref sets
`github.ref_type` to `branch`, which is enough to select the dry-run path below without touching
the `on:` block at all. Everything except the release attach and the rollback-target lookup is
ref-agnostic, and both of those are gated on `github.ref_type == 'tag'`, so a non-tag run resolves
`0.0.0-dryrun`, then signs, notarizes, staples and assesses for real while publishing nothing. A
temporary `push: branches:` trigger is not a second dry-run route, and that is not left to
discipline: the [[infrastructure#Naming Gate|naming gate]]'s class 3 assertion reds the branch
carrying one, which fails the called `ci.yml` gate and leaves `release` (`needs: gate`, no `if:`)
skipped before it signs anything.

### Refusing An Unrun Gate

The release calls [[infrastructure#Continuous Integration|`ci.yml`]] as its gate and reads all
four [[infrastructure#Continuous Integration#Gate-Ran Outputs|gate-ran outputs]] back before
anything is signed.

`needs: gate` alone is not a gate, because a skipped job reports success. The first step of the
release job therefore compares `cross_build`, `unit`, `naming` and `lat_check` against the
literal string `true` and names every gate that is anything else. The comparison is fail-closed:
an absent output reads as an empty string, so a gate deleted from `ci.yml`, renamed out from
under its caller, or skipped stops the release rather than passing silently. The gate job
inherits no secrets — every check it runs is Mac-less and credential-free.

### Signing Chain Facts

The chain was proven end to end on a throwaway hello-world daemon before any release pipeline was
written — see [[infrastructure#Release Chain#Credential Smoke Test]] — and two facts it pinned
down are load-bearing rather than incidental.

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

### Credential Smoke Test

`.github/workflows/notarize-spike.yml` is that same chain run against a throwaway hello-world
daemon, on `workflow_dispatch` only. It is the cheapest thing that can tell a maintainer an Apple
secret is wrong.

The proof it carries is identical in substance to the release path — one `security import` of the
two Developer ID identities, a hardened-runtime `codesign`, `pkgbuild` then `productsign`,
`notarytool submit --wait` against the `.pkg` itself, `stapler staple` and `validate`, and an
`spctl --assess --type install` verdict taken with the quarantine attribute deliberately on — and
it ends by installing the still-quarantined package and running the payload. What differs is the
payload: a hello-world `main.go` written into `$RUNNER_TEMP` by the workflow itself, so nothing in
this repository is built, signed, or shipped by it, and the spike can never become a second,
unreviewed release path for the real agent.

Its standing job is credential validation. A mis-encoded `APPLE_CERTIFICATE`, a wrong
`APPLE_CERTIFICATE_PASSWORD`, or a stale `APPLE_ID` / `APPLE_PASSWORD` / `APPLE_TEAM_ID` triple
surfaces here in minutes on a disposable artifact, instead of halfway through a release that has
already built and signed the real product — which is why it is run after any credential rotation
and before the first release from a freshly provisioned set of secrets. The certificate check
reports shape only, a decoded byte count and the DER header, because a public log has to be able
to distinguish "this is not a base64 PKCS#12" from "the password is wrong" while carrying
neither.

Like the release workflow it names no product string of its own: it sources
`packaging/identity.sh` for `BINARY_NAME` and `BUNDLE_ID` and derives `<BUNDLE_ID>.spike` as the
throwaway package identifier. That identifier is a different package from the product's own, so a
spike run can never leave a receipt the real installer would later mistake for its own. Dispatch
is manual because every run burns a notarization submission and a `macos-latest` runner while
gating nothing automatically.

### Asset Retention

The versioned installer asset is named `browser-print-agentd-<version>.pkg`, so every release
keeps its own installer and no prior notarized `.pkg` is ever overwritten. That is the retention
half of the documented downgrade path.

The exact name is asserted before upload rather than trusted to convention, because an
unversioned asset would make a downloaded installer ambiguous about which build it carries. One
version string drives the linker stamp, the package version `build-pkg.sh` hands `pkgbuild`, and
that asset name, so the release path can predict the package path and hard-fail when the script
writes something else instead of discovering whatever it happened to produce.

Only after the package passes notarization, stapling, and the quarantined Gatekeeper assessment
does the workflow derive two metadata assets from it. `<pkg>.sha256` is one `shasum`-compatible
line containing the lowercase package digest and exact installer asset name. The stable
`update-manifest.txt` asset is exactly three newline-terminated records:

```text
version=<X.Y.Z>
asset=<browser-print-agentd-X.Y.Z.pkg>
sha256=<64 lowercase hexadecimal characters>
```

The workflow constructs expected checksum and manifest files independently and compares them
byte-for-byte before upload. It also asserts all three asset names rather than trusting path
construction. The package, checksum, and manifest are uploaded together, and the release is
marked `--latest` explicitly so the updater's `releases/latest/download/…` URL resolves to the
feed that names that package.

Retention is only real if a run proves it, so the run that ships a build also names the build a
station falls back to: after upload the previous `v*` release is read back and its installer and
download URL recorded as this release's rollback target — the input
[[operations#Station Operations#Rollback Path]] consumes, and the reason retention is a gate rather
than a habit. That lookup selects the previous release's **versioned** `.pkg` and explicitly
excludes the [[infrastructure#Release Chain#Evergreen Download Asset|evergreen copy]]: a release
carries two `.pkg` assets, and a version-free filename names no build, so an `endswith(".pkg")`
match alone could report a rollback target that tells an operator nothing. Nothing in the path
deletes, and a `--clobber` on the upload can only ever replace the five assets of the tag being
released, since a different tag is a different release. A prior release with no versioned `.pkg`
warns rather than fails — the package just shipped is already published and notarized, and
failing there would report a bad release for a defect in an older one.

### Evergreen Download Asset

Every release also attaches the notarized installer a second time as `browser-print-agentd.pkg`,
a byte-identical copy under a version-free name. It is a public cross-project contract, and no
release may ship without it.

A downstream consumer — the operator-facing web app whose print surface talks to this agent —
hands a station with no agent installed a single `releases/latest/download/browser-print-agentd.pkg`
link. GitHub answers that with a 302 to whichever release is currently marked latest, so the URL
never has to be edited for a new version. That redirect ignores drafts and prereleases, which is
why the release is marked `--latest` only after all five assets are attached: the link must never
resolve to a release that is still missing its installer. Renaming the asset, or shipping without
it, 404s the only install route offered to a station that cannot print until it gets one.

The obligation is unconditional by construction. The copy step carries no `if:`, so no tag
pattern, workflow input, or manual gate can turn it off, and a `workflow_dispatch` dry run
exercises it too. The name comes from `packaging/identity.sh` rather than a workflow literal, so
it moves with a product rename like every other identity-derived string. The copy is taken after
notarization, stapling and the quarantined Gatekeeper verdict, and the workflow then compares its
SHA-256 against the versioned package and revalidates the stapled ticket on the copy itself —
this is the file an operator actually downloads, so it is checked as such rather than assumed
correct because `cp` returned zero.

The updater does not consume this asset. It reads `update-manifest.txt`, which names the
versioned package, so automatic updates keep working with the evergreen copy missing — which is
exactly what makes its absence easy to miss and why `RUNBOOK.md` documents an explicit
logged-out check after every release.
