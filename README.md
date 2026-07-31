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

## Wire contract

The contract is **frozen**. Paths, request and response shapes, status codes, plain-text error
bodies, and the CORS origin echo are compatibility surface and do not change.

| Method | Path         | Response                                                                                                                 |
| ------ | ------------ | ------------------------------------------------------------------------------------------------------------------------ |
| `GET`  | `/available` | `{"printer": [Device, …]}` — only queues that can actually print, USB before network, inside a 1500 ms probe budget       |
| `GET`  | `/default`   | one `Device` object, or an **empty body** when nothing is healthy (an empty JSON object here would break callers)         |
| `POST` | `/write`     | spools `{"data": "<raw ZPL>"}` to the requested (or resolved) printer; empty `200` on success, plain-text body on failure |
| `POST` | `/read`      | empty `200` — dead surface for most callers, kept so the agent stays a drop-in                                            |
| `GET`  | `/health`    | additive diagnostics: running version, origin posture, and **every** discovered queue with its health verdict             |

`OPTIONS` on any path answers the CORS preflight with `204`.

**Ports.** `9100` is plain HTTP; loopback is exempt from mixed-content blocking, so
Chromium-family browsers reach it directly from an HTTPS page. `9101` is TLS and exists only
when the station cert pair is present — Safari needs it. Both bind loopback only; this is a
bridge between the local browser and local CUPS, never a network service.

**Versioning.** Every response — 404s and preflights included — carries
`X-Print-Agent-Version`. That header and `GET /health` are the *only* places the running version
is reported: the `Device` shape must never grow a version field, because callers parse and pin
it. A binary built any way other than a tagged release reports `dev`.

**Origin posture.** With no `--origin-allow` configured the agent is `log-and-allow`: every
origin is recorded and permitted. Configure an allowlist and `/write` rejects any other origin
with `403` *before* any CUPS work happens.

## Install

Download the signed `.pkg` for your architecture from the
[releases page](https://github.com/sharaf-nassar/browser-print-agentd/releases) and install it
while the station account is logged in at the console:

```bash
sudo installer -pkg browser-print-agentd-<version>.pkg -target /
```

Everything else is root work the package scripts do for you:

- **`preinstall`** removes any prior install of this agent (including installs made under the
  older product name), removes a path-matched Zebra Browser Print install if one is present, and
  then proves ports 9100 and 9101 are actually free. Anything else holding those ports would
  make a `KeepAlive` agent crash-loop, so the install stops loudly instead.
- **`postinstall`** generates a per-station self-signed cert pair (CN/SAN `localhost`, EKU
  `serverAuth`) under `~/Library/Application Support/browser-print-agentd/`, trusts it in the
  **System** keychain, bootstraps the LaunchAgent into `gui/<uid>` so printing works without a
  logout, and then polls `http://127.0.0.1:9100/available`. A failed probe **fails the install**.

Installed layout:

| Path                                                                       | What                      |
| -------------------------------------------------------------------------- | ------------------------- |
| `/usr/local/bin/browser-print-agentd`                                      | the agent binary          |
| `/usr/local/bin/browser-print-agentd-uninstall`                            | the uninstaller           |
| `/usr/local/libexec/browser-print-agentd/launcher`                         | launchd entry point       |
| `/Library/LaunchAgents/io.github.sharaf-nassar.browser-print-agentd.plist` | the LaunchAgent           |
| `~/Library/Application Support/browser-print-agentd/`                      | `cert.pem` and `key.pem`  |
| `~/Library/Logs/browser-print-agentd/`                                     | agent log                 |

**Uninstall** removes all of it — job, plist, binary, launcher, keychain trust (matched by SHA-1
fingerprint, never by name), cert and log directories, and the installer receipt:

```bash
sudo /usr/local/bin/browser-print-agentd-uninstall
```

It deletes the log, so copy the log first if you are uninstalling because something was wrong.

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
5. attaches `browser-print-agentd-<version>.pkg` to the GitHub release and records the previous
   release as the documented rollback target.

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

