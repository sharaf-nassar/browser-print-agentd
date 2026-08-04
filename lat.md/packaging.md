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
`AGENT_PLIST_PATH`, updater script/label/plist paths, public update-status path, root updater support/log paths,
`SUPPORT_DIR_NAME`, `LOG_DIR_NAME`, `LOG_FILE_NAME`, `ENV_PREFIX`, `TARGET_USER_ENV`,
`LOG_PATH_ENV`, `TEMP_PREFIX`, the derived
GitHub release URL, `COMPONENT_PKG_NAME`, the uninstaller app's
`UNINSTALL_TITLE`/`UNINSTALL_APP_NAME`/`UNINSTALL_APP_PATH`/`UNINSTALL_APP_BUNDLE_ID`/`UNINSTALL_APP_EXEC`,
and the release-tag glob. Everything but the product
name itself and the reverse-DNS namespace prefix is derived, so a rename is a one-line edit.

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

The template set is `launchagent.plist.in`, `updater.plist.in`, `distribution.xml.in`,
`launcher.sh.in`, `updater.sh.in`, `uninstall.sh.in`, `scripts/preinstall.in`,
`scripts/postinstall.in`, `component.plist.in`, and the two that make up the uninstaller app
([[packaging#Packaging#Station Installer#Uninstaller#The Uninstaller App]]):
`uninstall-app-info.plist.in` and `uninstall-app.sh.in`. Rendering fills both launchd labels and
paths, the `distribution.xml`
`title`/`choice`/`pkg-ref` ids and component package name, installer/uninstaller paths, account and
root support/log directories, release URL, log-tag prefixes, target-user environment variable,
and `mktemp` prefixes. Generated plist names derive from their labels rather than carrying
hardcoded filenames.

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
`StandardOutPath`/`StandardErrorPath`. The launcher resolves `$HOME`, exports the identity-derived
log path, and sends otherwise-unused stdout/stderr to `/dev/null`; the daemon opens the path and
owns all normal output. Failures before the daemon logger exists go to unified logging through
`logger`. The plist is also the per-station configuration surface — ports, bind address, and the
optional origin allowlist are edited there and nowhere else, and no origin ships in the package.

### Request Log Ownership And Rotation

The agent must own its log descriptor and size rotation; neither `newsyslog` nor launchd output
redirection can enforce [[tools#Print Agent#Origin Posture#Request And Job Log Retention]].

Before daemon ownership, the packaged launcher opened `agent.log` with shell append redirection
and then `exec`ed the agent. A macOS 26.6 station inspection confirmed both stdout and stderr
remained open on that same per-user inode for the process lifetime. Renaming the path did not move
those descriptors to a new file: writes continued into the renamed archive until restart.

`newsyslog` is rejected for this topology. Its installed manual and dry-run parser establish that
configuration fields are whitespace-separated, with no quoting or escaping for a home path that
contains a space. A glob avoids naming one account but cannot supply each matched file's dynamic
owner and group. More importantly, the agent has no pid file or reopen signal: the `N` flag leaves
its descriptors on the archive, while a signal introduces a compression or rename race and a
service restart in the middle of a request. Its periodic system job also bounds only the size seen
when that job runs, not growth between runs.

launchd's `StandardOutPath` and `StandardErrorPath` only map descriptors to a static absolute
path; they have no rotation or retention keys and do not expand a per-user home. Sending records
to unified logging instead is also rejected because the system's global retention policy gives
this product no explicit size or age bound and changes the admin's plain-file audit workflow. A
second root timer that stops the agent, rotates, and restarts it merely recreates a less safe
daemon-owned rotator with additional root/user ownership and in-flight-job failure modes.

All post-launch normal output uses one writer opened by the daemon in the resolved account home.
Under the logger mutex it checks size before each complete line, closes the active descriptor,
deletes only `.7`, shifts archives by atomic rename, opens a new private active file, and appends
the pending line. Startup removes unsafe or oversize exact ring members and repairs private modes
before the first intentional write, so KeepAlive restarts restore the bound.

The Go runtime's crash descriptor is rebound whenever a new active file opens. Rebinding closes
the previous duplicate, so continuous output cannot keep a renamed archive growing; a crash racing
the brief close/rename/open sequence may finish on the old retained inode, and restart
normalization restores the bound. Pre-launch and log-open failures use unified logging because no
daemon-owned file is available. launchd stdout/stderr paths remain unset, and the launcher sends
the daemon's otherwise-unused process descriptors to `/dev/null`.

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

### Root Updater LaunchDaemon

The updater is a separate, short-lived root LaunchDaemon; the print agent remains an unprivileged
per-user LaunchAgent with no egress or update routes.

`${UPDATER_LABEL}` runs `${UPDATER_PATH}` in the system domain at load and every 3600 seconds,
with 0-300 seconds (up to 5 minutes) of script-level jitter. The load-time run waits up to 300
seconds for a console user instead of testing once, because launchd starts this job before
loginwindow has handed `/dev/console` to anybody and an immediate test would make every boot check
a no-op. The `v0.3.0` release-validation
baseline temporarily used a 60-second interval and 0-5 seconds of jitter. `v0.3.1` carried the
production values but its unconditional trust mutation failed in the noninteractive updater;
`v0.3.2` is the idempotent-trust recovery release and retains those production values. It has no
`KeepAlive`. Every check therefore starts from the script currently on disk, and `postinstall`
never boots out an already registered updater: a package replacement cannot kill the process
that invoked `installer`. Consequently, automatic installation of `v0.3.2` replaces the plist
but the loaded `v0.3.0` job retains its 60-second interval until a reboot or explicit
system-domain bootout/bootstrap. Bootstrap happens only after the print agent's own health probe,
preserves launchd's disabled override, and uses an install marker plus the updater's
`installer`-process check to prevent the RunAtLoad execution from nesting inside the package
installation that registered it.

The system support directory is root-owned mode 755 solely to provide traversal to one sanitized,
root-owned mode-644 status file. Its `updater/` child remains mode 700 and holds the detailed
last-run file, failed-version quarantine, and verified rollback package. The public file is
atomically replaced and carries only bounded timestamp/outcome/latest-version/quarantine facts;
it contains no package, quarantine list, Team ID, URL, credential, or updater control. The root
log directory is distinct from every account's agent log. All paths, both updater artifacts, the
release feed URL, and updater label derive from `identity.sh`; uninstall removes them with the
original payload.

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
`/usr/bin/openssl` is LibreSSL. The script bootstraps the job into `gui/<uid>` for the console user
(or `${TARGET_USER_ENV}` for an unattended install), proves the HTTP endpoint, then waits for the
HTTPS listener with a bounded `curl -k`. Only after listener readiness does a normal
`https://localhost:9101/available` request test actual SecureTransport trust. Success leaves the
**System** keychain unchanged, making reinstall, rollback, and background update idempotent. Failure
runs `security add-trusted-cert -d -r trustRoot -p ssl` — machine-wide, admin-owned, and restricted
to TLS evaluation — then requires the same normal request to pass. This behavioral check avoids
localized trust-setting output and `security verify-cert`. A failed HTTP, HTTPS-readiness, or
normal trust probe **fails the install**: reporting success while either browser path is dead is
the exact phantom success this agent exists to kill.

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
device URI rather than the queue name — are carried as a procedure in
[[operations#Station Operations#Migrating From A Predecessor Agent]].

### Uninstaller

`uninstall.sh.in` ships as `${UNINSTALLER_PATH}` and removes everything the installer put down,
so "uninstalled" is a checkable state rather than an assumption.

It boots out and disables the job, deletes the plist, binary, and launcher, drops the keychain
trust and deletes the cert **by SHA-1 fingerprint** (never by name, which would match any other
`localhost` cert on the station), removes the cert and log directories, forgets the receipt, and
confirms 9100/9101 came free. Log deletion remains deliberate after adopting the bounded ring: a
full uninstall should not leave origin and device audit data behind in an otherwise abandoned
account directory. One ordering trap is documented rather than designed around: the uninstaller
deletes the active log and every archive, so copy the directory first if the agent is being
removed because something was wrong.

One line in it is a trap that bit a real station. Stale processes are cleared with `pkill -f`,
which matches its pattern anywhere in a command line — and the uninstaller's own command line is
`${BINARY_PATH}-uninstall`, so a bare `pkill -f "${BINARY_PATH}"` matches the uninstaller itself.
It killed the run one line before the payload deletion, leaving stations with their launchd jobs
torn down and every file still on disk, and it did so on both the command-line and GUI paths. The
pattern now requires whitespace-or-end after the path, which the agent's command line satisfies and
`-uninstall` cannot. Anchoring with `^` would be the obvious fix and is wrong: the updater is a
shell script, so its command line begins with the interpreter rather than the path.

Everything the script does is per-account below the machine-wide payload, so a station with more
than one printing account needs one `--user <account>` run per account. When an admin runs it at
all is an operations question rather than a packaging one:
[[operations#Station Operations#Rollback Path|rolling a station back]] does not use it, an upgrade
does not either, and the one case that requires it first is migrating away to a differently-named
agent.

#### The Uninstaller App

The one installer package carries its own uninstaller, as an app in `/Applications`, because
somebody who wants to remove a product looks on their own Mac rather than on a downloads page.

There is deliberately no second package. An earlier revision shipped the GUI uninstaller as a
separate payload-free `.pkg` attached to each release, and that was wrong on the thing that matters
most: a user who had installed the product had nothing on the machine to remove it with except a
CLI binary in `/usr/local/bin`, which Spotlight does not surface. Shipping two packages for one
product also doubled the signing, notarization and asset-retention surface for no benefit the user
could see.

The app owns no removal logic. `${UNINSTALLER_PATH}` stays the single implementation of what
"uninstalled" means; the bundle is a confirm dialog and one `do shell script ... with administrator
privileges` in front of it, so the double-click and the command line cannot drift apart. That call
is macOS's own authorization prompt, which is why the product still installs no privileged helper
and nothing runs as root but the uninstaller itself. A cancelled password prompt is distinguished
from a failure, and the uninstaller's one non-zero exit — a port still held after removal — is
reported as "removed, but something still holds the port" rather than as a failed uninstall, because
the files are gone either way.

Two packaging details are load-bearing. `pkgbuild` synthesizes `BundleIsRelocatable=true` for any
bundle in a payload, which would let `installer` write the app over a copy found elsewhere on disk
and leave `/Applications` empty; `packaging/component.plist.in` pins it false, and is hand-written
rather than produced by pkgbuild's analyze pass so the layout stays assertable from a Linux
stage-only run. The bundle is also codesigned with the same Developer ID Application identity as
the binary, because notarization requires every signable item in the payload to carry one — its
executable is a shell script rather than a Mach-O, so the signature seals the bundle and its
`Info.plist`.

`uninstall.sh.in` removes the app along with everything else, so a completed uninstall leaves
nothing behind; on macOS an executing file unlinks cleanly, so deleting the bundle out from under a
run started by double-clicking it is safe.

A double hyphen is illegal inside an XML comment, which a distribution file or component plist full
of tool flag names invites. `productbuild` and `plutil` catch it, but only on macOS, so a
`STAGE_ONLY=1` run would pass a broken file straight to a release: `build-pkg.sh` runs
`xmllint --noout` over the distribution, the component plist and the app's `Info.plist` when
xmllint is present, and says loudly in its log when it is not.
