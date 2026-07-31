# Packaging

How a station installer is produced: one identity record, a tree of templates, and a build
script that renders them into a staging layout before any Apple tool sees it.

## Packaging Identity

`packaging/identity.sh` is the single source for every string that names the product, and
[[identity.go#productName]] is its Go mirror. Nothing else in the repository spells an install
path, a bundle id, or a binary name by hand.

The record is POSIX `sh` with no side effects, because it is sourced by both `build-pkg.sh` and
the naming gate. It carries `PRODUCT_NAME`, `PRODUCT_TITLE`, `BUNDLE_ID` (used verbatim as both
the launchd `Label` and the `productbuild` package identifier), `BINARY_NAME`, `BINARY_PATH`,
`UNINSTALLER_NAME`/`UNINSTALLER_PATH`, `LIBEXEC_DIR`, `LAUNCHER_PATH`, `PLIST_NAME`,
`AGENT_PLIST_PATH`, `SUPPORT_DIR_NAME`, `LOG_DIR_NAME`, `ENV_PREFIX`, `TARGET_USER_ENV`,
`TEMP_PREFIX`, `COMPONENT_PKG_NAME`, and the release-tag glob. Everything but the product name
itself and the reverse-DNS namespace prefix is derived, so a rename is a one-line edit.

The Go half cannot be templated — the binary is compiled, not rendered — so `productName` is
restated once in `identity.go` and pinned to `identity.sh:BINARY_NAME` by the naming gate. That
equality assertion is the only thing standing between the two halves and silent drift.

### Go Identity Derivation

The compiled half spells the product name once and derives every runtime string that carries it,
so a rename never becomes a sweep through the Go sources either.

[[identity.go#productName]] is the only literal. From it come the default per-user Application
Support directory in [[config.go#defaultCertDir]], the `flag.NewFlagSet` program name that heads
the usage text, the advertised `Device.provider`, and [[identity.go#envPrefix]] — the
`BROWSER_PRINT_AGENTD_*` environment namespace, computed as the product name upper-cased with
hyphens turned into underscores so it cannot drift from `identity.sh:ENV_PREFIX`.

[[identity.go#tempPrefix]] is the one value restated rather than derived, mirroring
`identity.sh:TEMP_PREFIX`. It is deliberately shorter than the product name so the Go spool files
(`browser-print-*.zpl`) and the installer's `mktemp` templates share one visibly common prefix in
`/tmp`, which is what makes an orphaned file attributable at a glance.

The environment rename is breaking on purpose and ships **no** compatibility reader for the
pre-extraction variable namespace: the agent is configured by the LaunchAgent plist the
installer writes, so a silent fallback would only keep a stale variable alive long enough to be
forgotten. The flag names (`--bind`, `--port`, `--https-port`, `--cert-dir`, `--origin-allow`)
and the `X-Print-Agent-Version` header are untouched by any of this.

### Rendered Packaging Templates

Every shipped packaging artifact is a `.in` template that `packaging/build-pkg.sh` renders with
`sed` into its staging directory. No artifact is copied verbatim, and none is runnable straight
from the repository.

The template set is `launchagent.plist.in`, `distribution.xml.in`, `launcher.sh.in`,
`uninstall.sh.in`, `scripts/preinstall.in`, and `scripts/postinstall.in`. Rendering fills the
launchd `Label`, the `distribution.xml` `title`/`choice`/`pkg-ref` ids and component package
name, the `preinstall`/`postinstall`/`uninstall` `LABEL` and installed paths, the per-account
Application Support and Logs directory names, the log-tag prefixes, the target-user environment
variable, and the `mktemp` prefixes. The generated LaunchAgent file is named `${BUNDLE_ID}.plist`
rather than carrying a hardcoded filename.

The architecture value `__HOST_ARCHITECTURES__` is substituted by the same pass; it is the one
build-derived placeholder rather than an identity field.

### Unrendered Placeholder Guard

A template that still carries a `__PLACEHOLDER__` after rendering fails the build immediately,
printing the offending file and line rather than shipping a literal placeholder into a launchd
plist or an installer script.

The check runs twice: once per file inside the render helper, and once as a sweep over the whole
staged tree so a future artifact that is copied rather than rendered cannot slip through. A
`STAGE_ONLY=1` run therefore produces a staging tree with zero unrendered placeholders, which is
what makes the layout assertable on Linux CI with no Apple hardware.

### Build Script Environment Interface

`packaging/build-pkg.sh` reads `VERSION`, `ARCH`, `OUTPUT_DIR`, `APP_SIGNING_IDENTITY`,
`INSTALLER_SIGNING_IDENTITY`, `STAGE_ONLY`, and `GO_LDFLAGS` from the environment, each with a
matching command-line flag.

This interface is deliberately stable: the release workflow invokes the script through it, so
adding template rendering changed what the script does without changing how it is called. With
no `VERSION`, the default is derived from the newest `v*` tag, falling back to `0.0.0-dev` in a
tree with no release tag. The staging directory is a `mktemp -d` under `browser-print-pkg`, and
the installer's cert generation uses `browser-print-openssl`, so temp files share one prefix.

## Station Installer

What the `.pkg` actually does to a station. It is a `.pkg` and not a drag-install `.dmg` because
everything install must do — remove the vendor's own Browser Print, trust a cert, register a
launchd job — is a root action.

### LaunchAgent, Not LaunchDaemon

The rendered `${BUNDLE_ID}.plist` is a **LaunchAgent**: the agent needs the operator's per-user
CUPS context to see the station's queues and binds loopback for the browser in that same login
session, and a system-context daemon can reach neither.

It is installed to `/Library/LaunchAgents`, which launchd still loads per user — one instance per
GUI login — so the `.pkg` can lay it down as a root-owned payload file while everything the agent
writes stays per-account: the cert pair under `[[config.go#defaultCertDir]]` and the log under
`~/Library/Logs/${LOG_DIR_NAME}`. Stations run one shared login, so in practice that is one cert
and one log per machine; moving to per-operator accounts is a re-run of the installer, not a
redesign.

`ProgramArguments` points at `${LIBEXEC_DIR}/launcher` (rendered from `launcher.sh.in`) rather
than straight at the binary, because launchd does not expand a home directory in
`StandardOutPath`/`StandardErrorPath`. The launcher resolves `$HOME` inside the user's own
session, creates the log directory, and appends the agent's stdout and stderr to `agent.log`;
failures that happen before the agent starts have nowhere to log, so they go to the unified log
through `logger`. The plist is also the per-station configuration surface — ports, bind address,
and the optional origin allowlist are edited there and nowhere else, and no origin ships in the
package.

### The launchd On-Demand Gate

`KeepAlive` is the unconditional boolean, and a 2026-07-30 station run established that this is
not a choice between plist shapes: **launchd gates a respawn on the login domain, not on the
job**.

When `gui/<uid>` is in on-demand-only mode — `launchctl print gui/<uid>` reports a nonzero
`on-demand count`, as it does while a restart-pending macOS update is staged — launchd refuses
every non-demand spawn and stamps the job with `pended nondemand spawn = <reason>`:
`speculative` for `RunAtLoad`, `inefficient` for a `KeepAlive` restart, `interval` for
`StartInterval`. `inefficient` is launchd's label for "configured to run constantly", not a power
verdict, which is why `caffeinate` does not clear it. On a station in that state the agent
survives neither a crash nor a logout. It was reproduced with a control job running nothing but
`/bin/sleep`, and `KeepAlive`-as-dict, every `ProcessType` value, dropping `ProcessType`,
`StartInterval` and `LimitLoadToSessionType` all pended identically — so no packaging change
fixes it.

The escapes are a demand-reason spawn (`launchctl kickstart`, an inbound socket connection, a
watch-path event) or the domain leaving on-demand-only mode, which is what installing the pending
update and rebooting does. The station confirmed that prediction the same day: once the operator
installed macOS Tahoe 26.6 and rebooted, `gui/501` no longer reported an `on-demand count` at
all, `RunAtLoad` brought the agent up unattended, and a `kill -9` of the running agent respawned
automatically in ~3 s — inside `ThrottleInterval=10`, with no plist edit involved. The gate is a
documented transient condition tied to a staged OS update, not a packaging defect, and keeping
stations current on macOS is therefore an availability requirement. The plist comment carries the
diagnosis so the next reader does not go hunting for a `KeepAlive` variant that does not exist.

### Preinstall And Postinstall

`scripts/preinstall.in` runs before a single file is written and does the things that can
invalidate the whole install; `scripts/postinstall.in` runs after the payload lands and does the
things that need it there.

`preinstall` first removes any prior copy of this product and then, by path,
the vendor's Browser Print: every matching launchd job is unloaded by the `Label` read out of its
own plist, leftover processes are killed, the app and support paths are deleted, and the receipts
are forgotten — nothing branches on the vendor's version or architecture, and a station that
never had it finds nothing to remove.
Then it proves 9100 and 9101 are actually free, which is the mitigation for the worst failure
mode: anything else holding those ports makes a `KeepAlive` agent crash-loop and printing die
silently, so the install stops loudly instead. Removal runs here rather than in `postinstall` so
a station that cannot be cleaned fails before the payload lands.

`postinstall` generates the per-station cert with `openssl` (CN and SAN `localhost`, EKU
`serverAuth`, 730 days — inside Apple's 825-day trust ceiling), reusing an existing pair unless it
is within 30 days of expiry so a reinstall does not churn trust for nothing. The recipe drives
OpenSSL through a config file and must not be rewritten to use `-addext`, because the station's
`/usr/bin/openssl` is LibreSSL. Trust goes into the **System** keychain via
`security add-trusted-cert -d -r trustRoot -p ssl`: machine-wide, admin-owned, and restricted to
TLS evaluation. It happens here, under root, precisely because operators may not have admin
rights. Since the agent starts `:9101` only when the pair exists, a failed cert step degrades to
"Safari cannot reach the agent", which a caller can detect, rather than to a silent break. The
script then bootstraps the job into `gui/<uid>` for the console user (or `${TARGET_USER_ENV}` for
an unattended install) so the station prints without a logout, and finally polls
`http://127.0.0.1:9100/available`. A failed probe **fails the install**: reporting success while
the agent is dead is the exact phantom success this agent exists to kill.

### Migration Is Not Automatic

Installing over a *differently-named* localhost print agent is a hard failure, not a migration.
`preinstall` stops only jobs it can identify — its own launchd label and the vendor's Browser
Print — so any other port holder trips the port-freedom check.

There is no legacy-cleanup path, and the omission is deliberate. Recognising a foreign agent means
guessing at a launchd label, an install path, and a keychain certificate that belong to a product
this one does not own; guessing wrong deletes somebody else's software, and guessing right only
duplicates the uninstaller that product already ships. Failing loudly with the `lsof` listing hands
the admin an unambiguous next step instead, which is the same bargain the port check makes
everywhere else: a station that looks installed but cannot print is worse than an install that
refused.

The consequences an admin has to plan for — uninstall the predecessor first, a freshly generated
and re-trusted station certificate because the cert directory is named after the product, and a
one-time printer re-pick because [[tools#Print Agent#Discovery And Stable Identity]] hashes the
device URI rather than the queue name — are written up in `RUNBOOK.md`.

### Uninstaller

`uninstall.sh.in` ships as `${UNINSTALLER_PATH}` and removes everything the installer put down,
so "uninstalled" is a checkable state rather than an assumption.

It boots out and disables the job, deletes the plist, binary, and launcher, drops the keychain
trust and deletes the cert **by SHA-1 fingerprint** (never by name, which would match any other
`localhost` cert on the station), removes the cert and log directories, forgets the receipt, and
confirms 9100/9101 came free. One ordering trap is documented rather than designed around: the
uninstaller deletes the log, so copy it first if the agent is being removed because something was
wrong.
