# browser-print-agentd

A localhost HTTP print agent for macOS. It listens on `127.0.0.1:9100` (and `127.0.0.1:9101`
over TLS), emulates the Zebra Browser Print wire contract, and spools raw ZPL through CUPS — so
a web page that already drives a label printer through Browser Print keeps working on a Mac with
no vendor software installed.

It is not a byte-for-byte clone of a vendor daemon. Three things are deliberately different:
printers are **discovered** from CUPS (`lpstat -v`) instead of hand-listed; every queue is
**health-checked at job initiation** with USB-to-network failover, so a job never reports "Sent"
into a dead printer; and every request's `Origin` is logged, with an optional allowlist enforced
on `/write`.

**Not affiliated with Zebra Technologies.** `browser-print-agentd` is an independent
reimplementation of a publicly observable localhost HTTP interface. It is not produced,
endorsed, sponsored, certified, or supported by Zebra Technologies Corporation. "Zebra",
"Browser Print", and "ZPL" are trademarks of their respective owners and appear here only to
name the wire contract this agent emulates.

**Running this on a station?** [`RUNBOOK.md`](./RUNBOOK.md) is the admin-facing guide: install,
migrate from another localhost print agent, roll back, uninstall, diagnose a station that will not
print, and validate one on real hardware.

## Wire contract

The first four rows are the **frozen** Zebra-compatible surface. Their paths, request and response
shapes, status codes, plain-text error bodies, and the CORS origin echo are compatibility surface
and do not change. The last two rows are **additive extensions** — they are not part of the frozen
contract, no caller of the frozen four is affected by their existence, and an agent that predates
them answers those paths with the plain-text `404` its default arm has always produced.

| Method | Path          | Response                                                                                                                 |
| ------ | ------------- | ------------------------------------------------------------------------------------------------------------------------ |
| `GET`  | `/available`  | `{"printer": [Device, …]}` — only queues that can actually print, USB before network, inside a 1500 ms probe budget       |
| `GET`  | `/default`    | one `Device` object, or an **empty body** when nothing is healthy (an empty JSON object here would break callers)         |
| `POST` | `/write`      | spools `{"data": "<raw ZPL>"}` to the requested (or resolved) printer; empty `200` on success, plain-text body on failure |
| `POST` | `/read`       | empty `200` — dead surface for most callers, kept so the agent stays a drop-in                                            |
| `GET`  | `/health`     | **additive** diagnostics: running version, origin posture, every queue's health, and updater status when safely available |
| `POST` | `/print-pdf`  | **additive**: spools `{"data": "<base64 PDF>"}` as a rendered document; same `200`/plain-text convention as `/write`      |

`OPTIONS` on any path answers the CORS preflight with `204`.

**`/print-pdf`.** For callers that render a multi-cell label *sheet* rather than one ZPL label.
It takes the same `{device, data}` envelope as `/write` and reuses the same printer resolution,
USB-to-network failover, and origin gating, so a sheet and a label can never disagree about which
printer is usable. `data` that will not base64-decode, or that decodes to bytes not beginning with
`%PDF`, is a `400` before any CUPS call; a body over 50 MB is a `413`. A PDF always runs through a
CUPS rendering chain — raw PDF bytes would reach the device unrendered and print as garbage. Most
queues receive the PDF as an ordinary document. For the one stock Zebra ZPL driver that emits an
inverted, device-stored graphic, the agent runs that queue's validated PPD offline, converts the
bounded bitmap to upright inline `^GFA`, and raw-spools only the generated printer-native ZPL. The
caller still sends the PDF the way up it wants it printed, and all other drivers remain untouched.

**Ports.** `9100` is plain HTTP; loopback is exempt from mixed-content blocking, so
Chromium-family browsers reach it directly from an HTTPS page. `9101` is TLS and exists only
when the station cert pair is present — Safari needs it. Both bind loopback only; this is a
bridge between the local browser and local CUPS, never a network service.

**Versioning.** Every response — 404s and preflights included — carries
`X-Print-Agent-Version`. That header and `GET /health` are the *only* places the running version
is reported: the `Device` shape must never grow a version field, because callers parse and pin
it. A binary built any way other than a tagged release reports `dev`.

**Update diagnostics.** When the packaged root updater has published valid local state,
`GET /health` adds an `update` object with its last-check time and outcome, the latest strictly
validated manifest version, whether that version is quarantined, and whether launchd currently
pins the updater disabled. The print agent makes no network request for this: it reads one
sanitized root-owned file and launchd's local disabled-state dictionary. Missing, malformed,
unsafe, or unprovable state omits `update` entirely. A check that ended before manifest
validation omits `latestVersion` and `quarantined` rather than guessing.

**Origin posture.** With no `--origin-allow` configured the agent is `log-and-allow`: every
origin is recorded and permitted. Configure an allowlist and both print routes — `/write` and
`/print-pdf` — reject any other origin with `403` *before* any CUPS work happens.

## Install

Download the signed `.pkg` for your architecture from the
[releases page](https://github.com/sharaf-nassar/browser-print-agentd/releases) and install it
while the station account is logged in at the console:

```bash
sudo installer -pkg browser-print-agentd-<version>.pkg -target /
```

Everything else is root work the package scripts do for you:

- **`preinstall`** removes any prior install of **this** agent, removes a path-matched Zebra
  Browser Print install if one is present, and then proves ports 9100 and 9101 are actually free.
  Anything else holding those ports — including a differently-named localhost print agent — would
  make a `KeepAlive` agent crash-loop, so the install stops loudly instead. Uninstall that agent
  first; see [the runbook](./RUNBOOK.md#migrating-from-another-localhost-print-agent).
- **`postinstall`** generates or reuses a per-station self-signed cert pair (CN/SAN `localhost`,
  EKU `serverAuth`) under `~/Library/Application Support/browser-print-agentd/`, bootstraps the
  LaunchAgent into `gui/<uid>`, and proves both listeners are ready. It then uses a normal
  `https://localhost:9101/available` request as the trust check: working **System** keychain trust
  is left untouched, while a first install adds SSL-only trust and requires that same request to
  succeed. It finally registers the separate root updater. A failed HTTP, HTTPS, or trust probe
  **fails the install**.

Installed layout:

| Path                                                                               | What                              |
| ---------------------------------------------------------------------------------- | --------------------------------- |
| `/usr/local/bin/browser-print-agentd`                                              | the agent binary                  |
| `/usr/local/bin/browser-print-agentd-uninstall`                                    | the uninstaller                   |
| `/usr/local/libexec/browser-print-agentd/launcher`                                 | agent launchd entry point         |
| `/usr/local/libexec/browser-print-agentd/updater`                                  | short-lived root updater          |
| `/Library/LaunchAgents/io.github.sharaf-nassar.browser-print-agentd.plist`         | per-user LaunchAgent              |
| `/Library/LaunchDaemons/io.github.sharaf-nassar.browser-print-agentd.updater.plist` | root updater LaunchDaemon         |
| `~/Library/Application Support/browser-print-agentd/`                              | `cert.pem` and `key.pem`          |
| `~/Library/Logs/browser-print-agentd/`                                             | private, bounded per-user log ring |
| `/Library/Application Support/browser-print-agentd/update-status`                  | sanitized updater diagnostics     |
| `/Library/Application Support/browser-print-agentd/updater/`                       | updater cache and state           |
| `/Library/Logs/browser-print-agentd/update.log`                                    | updater verification/install log |

The updater wakes at load and every 86400 seconds, adds 0-900 seconds (up to 15 minutes) of
per-run jitter, then exits after one check. The `v0.3.0` release-validation baseline alone used a
60-second interval with 0-5 seconds of jitter. `v0.3.1` carried the production values but failed
background installation when `postinstall` redundantly mutated already-working keychain trust;
`v0.3.2` is the idempotent-trust recovery release. When `v0.3.0` automatically installs
`v0.3.2`, launchd keeps the already loaded 60-second schedule until a reboot or an explicit
updater bootout/bootstrap, even though the plist on disk contains 86400. The updater does nothing
without a console user. A strict three-line manifest at GitHub's
`releases/latest/download` feed is authoritative: any version difference triggers an install,
including a downgrade when a bad latest release is yanked. Before replacement it caches and
verifies the currently installed release package; an install or version-probe failure restores
that package and quarantines the failed version from future attempts.

Its public status file is root-owned mode 644 beneath a root-owned mode-755 support directory.
The rollback cache, quarantine list, and detailed last-run state remain in the mode-700
`updater/` child. Pin state is never copied into a stale file: `/health` reads launchd live,
because a disabled updater cannot run again to rewrite its own publication.

Pin a managed or rolled-back station with one command; package upgrades preserve this disabled
override:

```bash
sudo launchctl disable system/io.github.sharaf-nassar.browser-print-agentd.updater
```

**Uninstall** removes all of it — both jobs and plists, binary, launcher, updater state/cache,
keychain trust (matched by SHA-1 fingerprint, never by name), cert and log directories, and the
installer receipt:

```bash
sudo /usr/local/bin/browser-print-agentd-uninstall
```

It deletes the log ring, so copy the directory first if you are uninstalling because something
was wrong.

**Configuration** is by flag, with an environment mirror for each:
`--bind`, `--port`, `--https-port`, `--cert-dir`, `--origin-allow`, mirrored by
`BROWSER_PRINT_AGENTD_BIND`, `_PORT`, `_HTTPS_PORT`, `_CERT_DIR`, and `_ORIGIN_ALLOW`. A flag
always wins over its environment mirror, which always wins over the built-in default. Printer
selection is deliberately **not** configurable — queues come from CUPS.

The agent does not create CUPS queues for you. Add the printer once with `lpadmin`
(`-m drv:///sample.drv/zebra.ppd`; `lpadmin -m raw` no longer exists on macOS).

## Build

Go 1.24, stdlib only — no third-party modules and no `go.sum`.

```bash
go build ./...          # build; the binary lands next to the sources
go vet ./...            # vet
go test -race ./...     # unit tests, race detector on
scripts/check-naming.sh # repository hygiene gate (see below)
```

The installer is built by `packaging/build-pkg.sh`, which cross-compiles the `darwin/<arch>`
binary, stages the payload and scripts, then runs `pkgbuild` → `productbuild` → optional
`productsign`. Stages 1 and 2 run anywhere Go runs; `--stage-only` stops before `pkgbuild`,
which is what makes the payload layout, file modes, and script set verifiable on Linux CI with
no Apple hardware.

```bash
packaging/build-pkg.sh --stage-only          # layout check, no macOS needed
packaging/build-pkg.sh --version 0.1.0       # full build (macOS)
```

Its environment interface, equivalent to the flags:

| Variable                     | Meaning                                                            |
| ---------------------------- | ------------------------------------------------------------------ |
| `VERSION`                    | package version (default: derived from the newest `vX.Y.Z` tag)     |
| `ARCH`                       | `arm64` (default) or `amd64`                                        |
| `OUTPUT_DIR`                 | where the `.pkg` lands (default `packaging/dist`)                   |
| `APP_SIGNING_IDENTITY`       | "Developer ID Application" identity; codesigns the binary           |
| `INSTALLER_SIGNING_IDENTITY` | "Developer ID Installer" identity; `productsign`s the package       |
| `STAGE_ONLY`                 | `1` to stop after staging                                           |
| `GO_LDFLAGS`                 | extra link flags; the release workflow injects `-X main.version=…`  |

Signing identities are opt-in rather than assumed, so a local build works unsigned — and an
unsigned build is named `-unsigned.pkg` so it cannot be mistaken for something shippable.

Product identity (binary name, bundle id, install directories) lives in exactly one place,
`packaging/identity.sh`, and every shipped packaging artifact is a `.in` template rendered from
it. `scripts/check-naming.sh` is the repository hygiene gate: it bans the originating org and
project names and every hardware or lab artifact outright, asserts `identity.sh` and
`identity.go` agree on the binary name,
asserts `X-Print-Agent-Version` is still present and unchanged, and asserts the release
workflow's trigger surface stays tag-only. It runs as a required CI job on every push and pull
request, and is callable locally with no arguments.

## Releases and signing

Releases are cut by tagging `v*.*.*` (`v0.1.0`, `v0.2.0`, …). Pushing the tag runs
`.github/workflows/release.yml`, which:

1. refuses to ship unless the CI gate — the `darwin/arm64` cross-build, the Go unit suite, the
   repository hygiene gate, and the `lat check` link check — actually ran and passed (a skipped
   job reports success, which is why that assertion exists);
2. builds and **codesigns** the binary with a Developer ID Application identity under the
   hardened runtime, then `productsign`s the distribution package with a Developer ID Installer
   identity;
3. submits the package to Apple **notarization** with `notarytool`, **staples** the ticket, and
   verifies with `spctl` that Gatekeeper reports `Notarized Developer ID`;
4. self-verifies that the packaged binary reports the tagged version on both `GET /health` and
   `X-Print-Agent-Version`, and that the asset name carries that version;
5. attaches `browser-print-agentd-<version>.pkg`, its `.sha256`, and
   `update-manifest.txt` to the GitHub release, marks that release latest explicitly, and records
   the previous release as the documented rollback target.

Nothing in that workflow deletes a tag, a release, or an asset — every shipped `.pkg` stays
independently downloadable, because rollback is exactly one step: install the previous release's
`.pkg` over the bad one.

Secrets the workflow needs:

| Secret                       | Purpose                                                              |
| ---------------------------- | -------------------------------------------------------------------- |
| `APPLE_CERTIFICATE`          | base64 PKCS#12 carrying **both** Developer ID identities              |
| `APPLE_CERTIFICATE_PASSWORD` | password for that PKCS#12                                             |
| `APPLE_ID`                   | Apple ID used for notarization submission                             |
| `APPLE_PASSWORD`             | app-specific password for that Apple ID                               |
| `APPLE_TEAM_ID`              | Developer Team ID the identities belong to                            |
| `KEYCHAIN_PASSWORD`          | optional; an ephemeral password is generated when it is unset         |

`.github/workflows/notarize-spike.yml` is a manual (`workflow_dispatch`) smoke test that proves
the whole signing chain against a throwaway package. Run it first after rotating any secret — it
is the cheapest way to find out that a pasted credential is wrong.

## License

[MIT](./LICENSE).
