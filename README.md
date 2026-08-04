# browser-print-agentd

**Print labels from a web page to a label printer on your Mac.**

Some web apps print labels by talking to a small helper program running on your own computer.
Zebra ships one for that job; this is a drop-in replacement for Macs that do not have it. You
install it once and label printing starts working in the browser — there is no account to create,
no window to keep open, and nothing to set up afterwards. It runs quietly in the background and
keeps itself up to date.

It only ever talks to your own Mac and to the printers already configured on it. It is not a
network service, nothing on the internet can reach it, and your labels are never sent anywhere.

**Not affiliated with Zebra Technologies.** `browser-print-agentd` is an independent
reimplementation of a publicly observable localhost HTTP interface. It is not produced,
endorsed, sponsored, certified, or supported by Zebra Technologies Corporation. "Zebra",
"Browser Print", and "ZPL" are trademarks of their respective owners and appear here only to
name the wire contract this agent emulates.

**Running this on a station?** [`RUNBOOK.md`](./RUNBOOK.md) is the admin-facing guide: install,
migrate from another localhost print agent, roll back, uninstall, diagnose a station that will not
print, and validate one on real hardware.

## Install

You need a Mac with Apple Silicon and your Mac's administrator password.

**[Download the installer](https://github.com/sharaf-nassar/browser-print-agentd/releases/latest/download/browser-print-agentd.pkg)**

Open the downloaded file, follow the prompts, and enter your Mac password when it asks. When it
finishes, label printing works. There is no application to launch and no next step.

A few things worth knowing:

- **Stay logged in while it installs.** It sets itself up under your own account, and stops with
  an error rather than half-finishing if you are not there.
- **The label printer must already be set up on this Mac.** This agent prints to the printers your
  Mac already has; it does not add them for you.
- **If the installer stops and mentions port 9100 or 9101**, another label-printing program is
  already running and has to be removed first — see
  [the runbook](./RUNBOOK.md#migrating-from-another-localhost-print-agent).

That link always points at the newest release. Every installer is signed and notarized by Apple,
so macOS will open it without warnings.

## Updates

The agent updates itself. It checks for a new version every hour and installs one in the
background when it finds it — you are never asked anything. Installing an update takes a few
seconds, during which printing is briefly unavailable.

**To force a check, restart the Mac.** Logging out and back in does not do it.

Administrators: the update cadence, how to pin a station to its current build, what the updater
verifies before installing anything, and how to roll a station back are all in
[`RUNBOOK.md`](./RUNBOOK.md#managing-automatic-updates).

## Uninstall

The installer puts an uninstaller on the Mac. Open **Uninstall Browser Print Agent** from
Applications — Spotlight finds it too — confirm, and enter your Mac password when asked. There is
nothing to download.

It removes all of it — both launchd jobs and their plists, the binary, the launcher, updater state
and cache, keychain trust (matched by SHA-1 fingerprint, never by name), the certificate and log
directories, the installer receipt, and itself. It deletes the log ring too, so copy that directory
first if you are uninstalling because something was wrong.

The same thing from Terminal, if you prefer:

```bash
sudo /usr/local/bin/browser-print-agentd-uninstall
```

Both run exactly the same code — the app is a confirm dialog and one administrator prompt in front
of the command above.

## What the installer does

Everything below is root work the package scripts do for you:

- **`preinstall`** removes any prior install of **this** agent, removes a path-matched Zebra
  Browser Print install if one is present, and then proves ports 9100 and 9101 are actually free.
  Anything else holding those ports — including a differently-named localhost print agent — would
  make a `KeepAlive` agent crash-loop, so the install stops loudly instead.
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
| `/Applications/Uninstall Browser Print Agent.app`                                  | GUI front end for the uninstaller |
| `/usr/local/libexec/browser-print-agentd/launcher`                                 | agent launchd entry point         |
| `/usr/local/libexec/browser-print-agentd/updater`                                  | short-lived root updater          |
| `/Library/LaunchAgents/io.github.sharaf-nassar.browser-print-agentd.plist`         | per-user LaunchAgent              |
| `/Library/LaunchDaemons/io.github.sharaf-nassar.browser-print-agentd.updater.plist` | root updater LaunchDaemon         |
| `~/Library/Application Support/browser-print-agentd/`                              | `cert.pem` and `key.pem`          |
| `~/Library/Logs/browser-print-agentd/`                                             | private, bounded per-user log ring |
| `/Library/Application Support/browser-print-agentd/update-status`                  | sanitized updater diagnostics     |
| `/Library/Application Support/browser-print-agentd/updater/`                       | updater cache and state           |
| `/Library/Logs/browser-print-agentd/update.log`                                    | updater verification/install log |

The updater's public status file is root-owned mode 644 beneath a root-owned mode-755 support
directory. The rollback cache, quarantine list, and detailed last-run state remain in the mode-700
`updater/` child. Pin state is never copied into a stale file: `/health` reads launchd live,
because a disabled updater cannot run again to rewrite its own publication.

## Configuration

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

Releases are cut by tagging `v*.*.*`; `RUNBOOK.md` and `lat.md/infrastructure.md` own the signing,
notarization, and asset-retention details.

## Wire contract

This agent is not a byte-for-byte clone of a vendor daemon. Three things are deliberately
different: printers are **discovered** from CUPS (`lpstat -v`) instead of hand-listed; every queue
is **health-checked at job initiation** with USB-to-network failover, so a job never reports "Sent"
into a dead printer; and every request's `Origin` is logged, with an optional allowlist enforced
on `/write`.

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

## License

[MIT](./LICENSE).
