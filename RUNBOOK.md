# browser-print-agentd — Station Runbook

Operational runbook for administrators. It covers what you do to a Mac that has to print labels:
install the agent, roll it back, take it off, migrate to it from another localhost print agent,
and find out why it is not printing.

`README.md` describes what the agent *is* — the frozen wire contract, the install layout, the
release chain. This document describes what you *do* with it. Read the wire contract first if you
have not; several diagnostics below only make sense against it.

The agent ships only as a signed, notarized `.pkg` attached to a `vX.Y.Z` GitHub release. Nothing
on a station is ever built from source, and no agent auto-updates itself: every version change on
a station is an admin installing a specific `.pkg` on purpose. That is what makes the rollback
below a one-step operation.

## Contents

- [Releases and where the installers live](#releases-and-where-the-installers-live)
- [Installing on a station](#installing-on-a-station)
- [Migrating from another localhost print agent](#migrating-from-another-localhost-print-agent)
- [Rolling back to the previous release](#rolling-back-to-the-previous-release)
- [Uninstalling](#uninstalling)
- [Diagnostics](#diagnostics)
- [Station validation checklist](#station-validation-checklist)

## Releases and where the installers live

Every release keeps its own installer, so any previously shipped version stays downloadable
forever. That retention is the precondition for the rollback path.

Releases are published by `.github/workflows/release.yml` under the tag that built them, and the
asset is named for its version:

| Tag      | Release asset                       |
| -------- | ----------------------------------- |
| `v1.4.0` | `browser-print-agentd-1.4.0.pkg`    |
| `v1.3.0` | `browser-print-agentd-1.3.0.pkg`    |

Three properties hold, and the release workflow enforces them rather than relying on convention:

- **The version is in the filename.** The workflow asserts the asset is exactly
  `browser-print-agentd-<version>.pkg` before uploading, so a downloaded installer is never
  ambiguous about which build it carries.
- **A release asset is scoped to its own tag.** The upload uses `--clobber` so that re-running a
  release for the _same_ tag replaces its own asset instead of failing; a different tag is a
  different release with a differently named asset, so no prior installer can be overwritten.
- **Nothing deletes.** The workflow never calls `gh release delete` or `gh release delete-asset`,
  and it prints the previous release's installer and download URL into its own job summary as the
  named rollback target for the build it just shipped.

List what is currently downloadable:

```bash
gh release list --repo sharaf-nassar/browser-print-agentd --limit 20
gh release view v1.3.0 --repo sharaf-nassar/browser-print-agentd \
  --json assets --jq '.assets[] | "\(.name)\t\(.size) bytes"'
```

The [releases page](https://github.com/sharaf-nassar/browser-print-agentd/releases) is the same
list in a browser, and is the only supported install source.

## Installing on a station

Installing is one `installer` command run by an admin while the station account is logged in at
the console. Everything else — removing Zebra Browser Print, generating and trusting the station
certificate, registering the LaunchAgent — happens inside the package's `preinstall` and
`postinstall` scripts, because all of it is root work and an operator may not have admin rights.

**Before you start**, have three things true:

- The station Mac is on a supported macOS and the **station account is logged in at the console**
  (not just SSH). The agent is a per-user LaunchAgent, so `postinstall` needs a real login session
  to put it in. For an unattended install with nobody at the keyboard, set
  `BROWSER_PRINT_AGENTD_TARGET_USER=<account>` in the installer's environment instead.
- The label printer is attached (USB or network) and its CUPS queue exists. The agent discovers
  queues, it never creates them — see [Adding a printer queue](#adding-a-printer-queue) below.
- Nothing else is listening on ports 9100 or 9101. If another localhost print agent is installed,
  read [Migrating from another localhost print agent](#migrating-from-another-localhost-print-agent)
  **before** you run the installer: the install will hard-fail, by design, rather than fight it
  for the ports.
- You are installing a `.pkg` downloaded from a GitHub release. Never a locally built one: a build
  named `…-unsigned.pkg` is not shippable and Gatekeeper will say so.

### 1. Download the release installer

```bash
gh release download v<version> --repo sharaf-nassar/browser-print-agentd \
  --pattern 'browser-print-agentd-*.pkg' --dir ~/Downloads
```

### 2. Install it

```bash
sudo installer -pkg ~/Downloads/browser-print-agentd-<version>.pkg -target /
```

The package is signed with a Developer ID Installer certificate, notarized, and stapled, so it
installs **Gatekeeper-clean with no `xattr -d com.apple.quarantine` and no right-click → Open**.
If macOS refuses the package, that is a real signal — stop and check the download rather than
working around it:

```bash
spctl --assess --type install -vv ~/Downloads/browser-print-agentd-<version>.pkg
pkgutil --check-signature ~/Downloads/browser-print-agentd-<version>.pkg
```

`-target /` is required. The package installs to absolute paths, and both scripts refuse any other
target volume.

### 3. What the install actually did

`preinstall` runs first, and any step failing stops the install before a single file lands:

- **Any prior copy of this agent is booted out**, by the launchd label read from its own plist, so
  a reinstall or an upgrade is never blocked by the version it is replacing.
- **Zebra Browser Print is removed, path-based.** Its launchd jobs are booted out by the `Label`
  read from each plist, leftover processes are killed, the app and support directories are
  deleted, and the receipts are forgotten. Nothing branches on Zebra's version or architecture, and
  a station that never had it simply finds nothing to remove. Coexistence is not a supported state:
  both agents want ports 9100/9101.
- **Ports 9100 and 9101 are proven free.** After the two removals above, the script waits up to
  10 s for the ports to drain and **fails loudly** if anything still listens, printing the `lsof`
  listing. That is deliberate: behind `KeepAlive`, an agent that cannot bind crash-loops and
  printing dies silently. Note the scope — the only things stopped for you are this agent and
  Zebra Browser Print. Any *other* port holder, including a differently-named print agent, is a
  hard failure you resolve first.

`postinstall` then does four things, in order:

1. **Generates the station certificate** with `openssl` into
   `~/Library/Application Support/browser-print-agentd/` (`cert.pem` + `key.pem`, mode 600 on the
   key), `CN=localhost`, `SAN=DNS:localhost,IP:127.0.0.1,IP:::1`, EKU `serverAuth`, 730 days —
   inside Apple's 825-day trust ceiling. An existing pair is **reused** unless it is within 30 days
   of expiry, so a reinstall or a rollback does not churn trust.
2. **Trusts that certificate** in the **System** keychain with
   `security add-trusted-cert -d -r trustRoot -p ssl`: machine-wide, admin-owned, and limited to
   TLS server evaluation. This is why an operator is never asked to trust a certificate, and it
   runs silently under the installer's root context — no password prompt, no keychain panel.
3. **Bootstraps the LaunchAgent** into `gui/<uid>` from `/Library/LaunchAgents`, so the station
   prints immediately with no logout and launchd starts it again at every login.
4. **Probes `http://127.0.0.1:9100/available` and fails the install if it does not answer** within
   ~15 s, dumping `launchctl print` state and the tail of the agent log. An install that reports
   success while the agent is dead is the exact phantom success this agent exists to kill.

### 4. Confirm the station really prints

```bash
curl -fsS  http://127.0.0.1:9100/health       # version + every queue, healthy or not
curl -fsS  http://127.0.0.1:9100/available    # what the caller will be offered, healthy only
curl -fsSk https://127.0.0.1:9101/available   # Safari path; only served when the cert exists
launchctl print gui/$(id -u)/io.github.sharaf-nassar.browser-print-agentd | head -20
```

Then print one real label from the page that drives the printer. `postinstall` already refused to
succeed with a dead agent, so a clean `installer` run plus a healthy `/health` is strong evidence
— but "CUPS accepted the job" is where the agent's honesty guarantee ends. **The physical label is
what closes the loop.**

On Safari, also open `https://localhost:9101/available` once in a tab: a printer list means the
trust step took. A security warning instead means the certificate is served but not trusted — see
[Safari and the certificate](#safari-and-the-certificate). Never ask an operator to click through
that warning; re-running the installer is the supported repair.

### Per-station configuration

`/Library/LaunchAgents/io.github.sharaf-nassar.browser-print-agentd.plist` is the only
configuration surface. Ports and bind address live in `ProgramArguments`; to lock the print routes
(`/write` and `/print-pdf`) to one origin, append two more strings — `--origin-allow` and the
allowed origin — then reload:

```bash
sudo launchctl bootout gui/$(id -u)/io.github.sharaf-nassar.browser-print-agentd
sudo launchctl bootstrap gui/$(id -u) \
  /Library/LaunchAgents/io.github.sharaf-nassar.browser-print-agentd.plist
```

Every flag also has an environment mirror (`BROWSER_PRINT_AGENTD_BIND`, `…_PORT`, `…_HTTPS_PORT`,
`…_CERT_DIR`, `…_ORIGIN_ALLOW`); a flag always wins. Leaving `--origin-allow` out is the default
posture: every origin is **logged and allowed**. Which printer to use is not configurable at all —
the agent discovers queues from CUPS.

### Adding a printer queue

The agent never creates queues. On a station with none, create one **without** a raw driver, which
macOS no longer supports:

```bash
lpadmin -p ZTC-ZD621-300dpi-ZPL -E \
  -v 'usb://Zebra%20Technologies/ZTC%20ZD621-300dpi%20ZPL?serial=<serial>' \
  -m drv:///sample.drv/zebra.ppd
lpstat -v          # confirm the device URI the agent will hash into the uid
```

`lpadmin -m raw` exits 1 with `Raw queues are no longer supported on macOS.` The `zebra.ppd` queue
is correct because the agent always spools with `lp -o raw`, which bypasses the filter.

## Migrating from another localhost print agent

Read this if the station already runs a *different* localhost print agent on 9100/9101 — a
predecessor of this one built under another product name, or any third-party build of the same
idea. The install does **not** migrate it for you, and that is a deliberate choice: the installer
only stops jobs it can identify as its own or as Zebra Browser Print, and it will not go hunting
for arbitrary port holders to kill.

### What happens if you skip this

`preinstall` reaches its port-freedom check with the other agent still listening, and stops:

```text
[browser-print-agentd preinstall] ERROR: port 9100 is still held after 10 s (see the listing
above). The agent would crash-loop behind a KeepAlive restart and printing would fail silently,
so this install is stopping here. Stop that process (or finish removing Zebra Browser Print by
hand) and re-run the installer.
```

Nothing has been written at that point — the station is exactly as it was. The fix is to remove
the other agent and re-run the installer; there is no flag that forces the install past this.

### The procedure

1. **Identify what is holding the ports**, so you remove the right thing:

   ```bash
   lsof -nP -iTCP:9100 -sTCP:LISTEN
   lsof -nP -iTCP:9101 -sTCP:LISTEN
   ```

   The `COMMAND` and the process's binary path name the product. Cross-check against
   [Telling two agents apart](#telling-two-agents-apart) below.

2. **Uninstall the other agent with its own uninstaller.** Every product's uninstaller is the only
   thing that knows its own launchd label, install paths, keychain trust, and receipt. Do not
   improvise with `kill` and `rm`: a launchd job that is deleted but not booted out comes back, and
   an orphaned trusted certificate stays trusted.

   Copy its log first if you are migrating because something was wrong — most uninstallers,
   including this one, delete the log.

3. **Confirm the ground is clear**, which is exactly the condition `preinstall` is about to check:

   ```bash
   lsof -nP -iTCP:9100 -sTCP:LISTEN ; lsof -nP -iTCP:9101 -sTCP:LISTEN   # expect no output
   ```

4. **Install this agent** per [Installing on a station](#installing-on-a-station). Nothing about
   the install is special because a predecessor was there — it is a normal first install onto a
   station that now has free ports.

### The certificate is re-trusted, once

A predecessor under a different product name kept its cert pair in its own
`~/Library/Application Support/<its-name>/` directory. This agent reads
`~/Library/Application Support/browser-print-agentd/`, finds nothing, and `postinstall` therefore
**generates a fresh pair and trusts it** in the System keychain. That is the intended path and it
is silent — but it means the station's Safari trust for `https://localhost:9101` is established
once more, against a new certificate.

Two consequences worth checking after a migration:

- **Verify the new trust took.** Open `https://localhost:9101/available` in Safari once. A printer
  list means `security add-trusted-cert` landed; a warning means it did not, and re-running the
  installer is the repair.
- **The predecessor's certificate may still be trusted.** Its uninstaller should have removed it
  (this one matches by SHA-1 fingerprint, never by name, precisely so it cannot delete somebody
  else's `localhost` cert). Confirm the System keychain does not carry a stale one:

  ```bash
  security find-certificate -c localhost -a /Library/Keychains/System.keychain
  ```

### The caller re-picks its printer, once

This agent derives each device `uid` by hashing the raw CUPS device URI. A predecessor that keyed
on the queue name, or that hashed the URI differently, produced different uids — so a printer
pinned in the browser before the migration will not match after it, and the calling page will ask
the operator to choose once. That is expected, not a fault. Clearing the caller's stored pin has
the same effect and is the faster fix if a stale pin is causing a prompt loop.

### Telling two agents apart

Two builds of this product family can both report version `0.1.0`, and the wire contract is frozen
— `X-Print-Agent-Version` is the only version channel the product has, and the `Device` shape can
never grow a field to carry a product name. **Do not identify an installed agent by its version
string.** Identify it by where it lives:

| What to check      | Command                                                             | This agent                                       |
| ------------------ | ------------------------------------------------------------------- | ------------------------------------------------ |
| Binary on disk     | `lsof -nP -iTCP:9100 -sTCP:LISTEN`                                  | `/usr/local/bin/browser-print-agentd`            |
| launchd label      | `launchctl print gui/$(id -u) \| grep -i print`                     | `io.github.sharaf-nassar.browser-print-agentd`   |
| Installer receipt  | `pkgutil --pkg-info io.github.sharaf-nassar.browser-print-agentd`   | a receipt with a version and install date        |
| Advertised product | `curl -fsS http://127.0.0.1:9100/available`                         | every `Device` has `"provider":"browser-print-agentd"` |
| Uninstaller        | `ls -l /usr/local/bin/browser-print-agentd-uninstall`               | present                                          |

`pkgutil --pkgs | grep -i print` lists every print-related receipt on the station in one line, which
is the fastest way to discover that a predecessor is still registered.

## Rolling back to the previous release

**When to use this:** a station took a new agent version and something is wrong that is not a
printer, a queue, or a network problem — printing broke, `/available` stopped answering, Safari
lost `:9101`, or the agent is crash-looping. Reinstalling the previous release's `.pkg` restores
service. There is no un-install step and no cleanup first: the older installer is a normal install
that happens to be older.

1. **Record what is running**, so the bad build is identifiable after it is gone:

   ```bash
   curl -fsS http://127.0.0.1:9100/health
   pkgutil --pkg-info io.github.sharaf-nassar.browser-print-agentd
   ```

   `GET /health` reports the running version, and every response also carries it as
   `X-Print-Agent-Version`. Copy the agent log before overwriting it —
   `~/Library/Logs/browser-print-agentd/agent.log` — because the whole point of rolling back is
   that someone still has to diagnose the bad build afterwards.

2. **Download the previous release's installer** from its own GitHub release. The asset name
   carries the version, so downgrading is picking a filename:

   ```bash
   gh release download v<previous> --repo sharaf-nassar/browser-print-agentd \
     --pattern 'browser-print-agentd-*.pkg' --dir ~/Downloads
   ```

   Any release that ever shipped is a valid target; there is nothing special about the immediately
   preceding one.

3. **Install it over the bad build.** No uninstall first:

   ```bash
   sudo installer -pkg ~/Downloads/browser-print-agentd-<previous>.pkg -target /
   ```

   This works because of three deliberate properties of the package:
   - `preinstall` **boots out any prior copy of this agent** before it checks that ports 9100 and
     9101 are free, so the running bad build does not block its own replacement. (Anything _else_
     holding those ports still fails the install loudly — that is a different problem, and
     [Migrating from another localhost print agent](#migrating-from-another-localhost-print-agent)
     is where it is handled.)
   - The package carries **no downgrade guard**. `distribution.xml` gates on host architecture and
     minimum macOS version only, so `installer` lays down an older payload over a newer one without
     complaint.
   - `postinstall` **reuses the existing station cert** unless it is within 30 days of expiry, so a
     rollback does not re-trust a new cert, does not re-prompt, and does not disturb Safari's view
     of `https://localhost:9101`.

4. **Confirm the rollback took**, on the station, before handing it back to an operator:

   ```bash
   curl -fsS http://127.0.0.1:9100/health          # version == the release you just installed
   curl -fsS http://127.0.0.1:9100/available       # the station's printers, as the caller sees them
   curl -fsSk https://127.0.0.1:9101/available     # only if the station uses Safari
   ```

   Then print one real label. `postinstall` already fails the install if the agent does not answer
   on 9100, so a successful `installer` run plus a healthy `/health` is strong evidence — but "CUPS
   accepted the job" is where the agent's honesty guarantee ends, so the physical label is what
   closes the loop.

5. **Pin the station until the bad build is fixed.** Nothing auto-updates, so a rolled-back station
   stays on the older version until an admin installs a newer `.pkg` by hand. File the defect
   against the bad release before moving on.

**Rolling back across a rename.** If the version you want to go back to shipped under a *different*
product name, this is not a rollback — it is a migration in the other direction. Its `.pkg` will
hit the same port-freedom hard failure, so uninstall this agent first with
`browser-print-agentd-uninstall`, then follow
[Migrating from another localhost print agent](#migrating-from-another-localhost-print-agent) in
reverse.

> **Retention is enforced; the restore is not yet hardware-validated.** The retention half of this
> path — a prior notarized `.pkg` stays independently downloadable per release — is enforced by the
> release workflow. The "reinstall the prior `.pkg` restores a working agent" half is validated
> once, on a real station, by
> [checklist item 11](#11-downgrade--reinstall-the-prior-pkg). Record the observed result here when
> it is run — this note is the only place in the repository that tracks that standing, so there is
> nothing else to update.

## Uninstalling

`browser-print-agentd-uninstall` ships as `/usr/local/bin/browser-print-agentd-uninstall` and
removes everything the install put on the machine: the LaunchAgent, the binary and launcher, the
keychain trust and the station cert (matched by SHA-1 fingerprint, never by name), the cert and log
directories, and the installer receipt — then confirms 9100 and 9101 came free.

```bash
sudo browser-print-agentd-uninstall          # add --user <account> on a multi-account station
```

Two flags matter:

- `--user <account>` operates on that account rather than the console user. Cert and log paths are
  per-account, so a station with more than one printing account needs **one run per account**. Run
  without a console user and the script warns that it removed the machine-wide files only.
- `--keep-cert` leaves the pair on disk. It is still untrusted and still unusable — the flag exists
  for an admin who wants to inspect the cert before it disappears.

Confirm the station is clean:

```bash
launchctl print gui/$(id -u)/io.github.sharaf-nassar.browser-print-agentd  # "Could not find service"
lsof -nP -iTCP:9100 -sTCP:LISTEN ; lsof -nP -iTCP:9101 -sTCP:LISTEN        # expect no output
pkgutil --pkg-info io.github.sharaf-nassar.browser-print-agentd            # expect "No receipt"
security find-certificate -c localhost -a /Library/Keychains/System.keychain  # ours is gone
ls ~/Library/Logs/browser-print-agentd 2>/dev/null                         # gone unless you kept a copy
```

Copy `~/Library/Logs/browser-print-agentd/agent.log` first if you are uninstalling because
something was wrong — the uninstaller deletes it.

Uninstalling is **not** part of the rollback path: reinstall the older `.pkg` directly. Nor is it
part of upgrading; a newer `.pkg` installs straight over an older one. It **is** the first step of
migrating away to a differently-named agent, because that agent's installer will hard-fail on the
ports otherwise.

## Diagnostics

The station will not print. Work down this list in order — each step rules out one layer, and the
first four take under a minute.

### The 60-second triage

```bash
curl -fsS http://127.0.0.1:9100/health
```

| What you get                                | What it means                                                   | Go to                                                |
| ------------------------------------------- | ----------------------------------------------------------------- | ---------------------------------------------------- |
| Connection refused                          | The agent is not running or not bound                           | [Agent not answering](#agent-not-answering)          |
| 200, `printers: []`                         | Agent is fine; CUPS has no queue at all                         | [CUPS side](#the-cups-side)                          |
| 200, the queue listed with `healthy: false` | Agent is fine; the printer is disabled, rejecting, or wedged    | [CUPS side](#the-cups-side)                          |
| 200, a CUPS error alongside the version     | Agent is alive and blind to CUPS — that is itself the diagnosis | [CUPS side](#the-cups-side)                          |
| 200 and healthy, but the caller disagrees   | Browser-side: cert, port, or pinned printer                     | [Browser side](#the-browser-side)                    |
| 200 and healthy, and the caller says "Sent" | The job reached CUPS; the failure is past the agent             | [The job left, no label](#the-job-left-but-no-label) |
| A label printed, but upside down            | The queue's driver flips the page and was not recognised        | [Upside down](#a-pdf-label-prints-upside-down)       |

`GET /health` is always the first call. Unlike `/available`, which hides unhealthy printers so a
caller can never pin one, `/health` lists **every** discovered queue with its verdict, and it
answers 200 even when CUPS itself is unreachable. The same version rides every response as the
`X-Print-Agent-Version` header.

If the agent answers but you are not sure it is *this* agent, see
[Telling two agents apart](#telling-two-agents-apart).

### Agent not answering

```bash
launchctl print gui/$(id -u)/io.github.sharaf-nassar.browser-print-agentd | head -30
tail -50 ~/Library/Logs/browser-print-agentd/agent.log
lsof -nP -iTCP:9100 -sTCP:LISTEN
lsof -nP -iTCP:9101 -sTCP:LISTEN
```

- **`Could not find service`** — the job is not registered. Either the install did not finish or
  someone booted it out. Re-run the installer; that is the supported repair.
- **Registered but the log shows repeated startup lines every ~10 s** — it is crash-looping behind
  `KeepAlive` + `ThrottleInterval 10`. Read the last error before each restart. Two causes
  dominate: something else holds 9100/9101 (the `lsof` output names it — leftover Zebra Browser
  Print is the classic), or `lp`/`lpstat` are missing, which the agent treats as fatal on purpose
  because an agent that cannot reach CUPS could only ever report phantom success.
- **The port is held by a process that is not ours** — stop it and re-run the installer.
  `preinstall` would have caught this at install time, so the conflict arrived afterwards.
- **9100 answers but 9101 does not** — expected when the certificate pair is absent. The agent
  starts the TLS listener **only** when `~/Library/Application Support/browser-print-agentd/` holds
  both `cert.pem` and `key.pem`. Chromium stations keep working; Safari does not. Go to
  [Safari and the certificate](#safari-and-the-certificate).
- **Nothing in the log at all** — the failure happened before the agent started, so it went to the
  unified log instead:
  `log show --last 30m --predicate 'process == "logger"' | grep browser-print`.
- **Registered, `state = not running`, and a `pended nondemand spawn` line** — launchd is refusing
  to start it. This is not an agent fault and re-running the installer will not help; go to
  [The agent died and launchd never restarted it](#the-agent-died-and-launchd-never-restarted-it).

### The agent died and launchd never restarted it

`KeepAlive` is not unconditional. launchd will hold a restart indefinitely when the **login
domain** is in on-demand-only mode, and the symptom is a station that stays down until someone
intervenes. Confirm it in two commands:

```bash
launchctl print gui/$(id -u)/io.github.sharaf-nassar.browser-print-agentd | grep -E 'state|pended'
launchctl print gui/$(id -u) | grep 'on-demand count'
```

A job stuck at `state = not running` with a `pended nondemand spawn = <reason>` line, in a domain
whose `on-demand count` is **nonzero**, is this condition. The reason names the spawn launchd
turned down — `speculative` for `RunAtLoad`, `inefficient` for a `KeepAlive` restart, `interval`
for `StartInterval`. Read `inefficient` literally: it is launchd's word for "this job is configured
to run constantly", not a power-management judgement, which is why `caffeinate` does nothing for
it.

**Get the station printing again** — a `kickstart` is a demand spawn, so it is not subject to the
gate and takes effect immediately:

```bash
launchctl kickstart gui/$(id -u)/io.github.sharaf-nassar.browser-print-agentd
curl -fsS http://127.0.0.1:9100/health
```

**Then clear the condition, because until you do, the agent will not survive its next crash and
will not come back at the next login either.** The trigger observed in the field is a
**restart-pending macOS update**: while one is staged, the login domain sits in on-demand-only
mode. Check and finish it:

```bash
softwareupdate --list          # an entry with 'Action: restart' is the one that matters
ls /Library/Updates            # staged product directories
```

Install the update and reboot the station. Do this during a maintenance window, not mid-shift.
After the reboot, `on-demand count` should be absent or `0`, and the `kill -9` check in
[item 6 of the validation checklist](#6-sleep-wake-logout-reboot-crash) should pass.

Do not go looking for a plist fix. This was measured on a station in that state on macOS 26.4
(build 25E246): `KeepAlive` as a dict, `ProcessType` set to `Standard`, `Background` or removed
altogether, an added `StartInterval`, and `LimitLoadToSessionType` were each bootstrapped as a
separate test job and every one of them pended identically — as did a control agent whose only
program was `/bin/sleep`. The gate does not read the plist. What it does read is the domain, and
the only escapes are a demand-reason spawn (`kickstart`, an incoming socket connection, a
watch-path event) or the domain leaving on-demand-only mode.

The remediation was then confirmed on the same station: after the pending macOS update was
installed and the machine rebooted, `launchctl print gui/<uid>` no longer showed an
`on-demand count` line at all, the agent auto-started at login through `RunAtLoad` with nobody
kickstarting it, and a `kill -9` of the running agent was followed by an automatic launchd respawn
in **~3 s** — well inside `ThrottleInterval=10`. No plist change was involved. This is a documented
transient condition tied to a staged OS update, not a standing defect.

### The CUPS side

The agent reports what CUPS tells it. When `/health` says a queue is unhealthy, confirm it
first-hand — and note that **the exit code carries almost nothing**: a _disabled_ queue exits 0
from `lpstat -p`, only an _absent_ one exits 1.

```bash
lpstat -v                      # device URIs — the agent hashes these into each uid
lpstat -p ZTC-ZD621-300dpi-ZPL # "enabled since" vs "disabled since" — read the TEXT
lpstat -a ZTC-ZD621-300dpi-ZPL # "accepting requests" vs "not accepting requests"
lpstat -o                      # jobs stuck in the queue
```

A queue is usable only when it is **present, enabled, AND accepting**. All three are checked
because CUPS hides two separate failures, both of which were confirmed on real hardware rather than
inferred from the man pages:

- **A disabled queue still exits 0 from `lpstat -p`.** Only an absent queue exits 1, so the exit
  code carries exactly one bit and health has to be read from the printed text.
- **A `cupsreject`ed queue is reported by `lpstat -p` as `enabled`.** It prints
  `is idle. … Rejecting Jobs` rather than hiding the queue — so `lpstat -p` does not merely omit a
  rejecting queue, it actively describes it as fine. The separate `lpstat -a` check is mandatory
  for exactly this reason. A station diagnosed by `lpstat -p` alone will call a rejecting queue
  healthy.

| Symptom                                  | Fix                                                                                                                                           |
| ---------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| `disabled since …`                       | `cupsenable <queue>` — then find out what disabled it                                                                                         |
| `not accepting requests since …`         | `cupsaccept <queue>`                                                                                                                          |
| No `lpstat -v` row at all                | The queue does not exist; see [Adding a printer queue](#adding-a-printer-queue)                                                               |
| Row exists, printer physically unplugged | Correct behaviour: the agent omits it from `/available`                                                                                       |
| `lpstat` itself hangs                    | A wedged USB device. Each probe is bounded at 900 ms, so the agent reports it unhealthy rather than hanging — the printer needs a power cycle  |

Then prove the CUPS path end-to-end, bypassing the agent entirely:

```bash
printf '^XA^FO50,50^A0N,40,40^FDtest^FS^XZ' > /tmp/test.zpl
lp -d ZTC-ZD621-300dpi-ZPL -o raw /tmp/test.zpl
```

**`-o raw` is load-bearing, not an optimization.** Without it the `zebra.ppd` filter rasterizes the
ZPL into a `~DGR:CUPS.GRF` bitmap and the label prints as a _picture of itself_ — no error, just
the wrong output. If this command prints and the calling page does not, the problem is above CUPS.
If it does not print, the problem is CUPS or the printer, and the agent is reporting honestly.

### The browser side

- **Chrome / Edge** talk to `http://localhost:9100` directly; loopback is exempt from
  mixed-content blocking, so there is no certificate step and never was one.
- **Safari** refuses plain loopback from an https page and uses `https://localhost:9101` only.
- A caller that races both candidates typically aborts after **1500 ms**, so "agent unreachable" in
  the page means _both_ failed inside that budget. That is also the budget the agent's own
  concurrent, per-probe-bounded health check is designed to stay inside.
- **The wrong printer, or none offered** — the caller stores the chosen `uid` itself, usually in
  `localStorage`, and that pin is per-browser-profile and per-station by design. Clearing it makes
  the caller re-pick on the next successful print.
- **A pre-existing pin from another agent will not match.** The agent derives each `uid` by hashing
  the raw device URI, so an operator re-picks the printer exactly once after cutover. That is
  expected, not a bug — see
  [The caller re-picks its printer, once](#the-caller-re-picks-its-printer-once).
- **Two printers offered and a prompt appears** — correct, if the caller prompts on ambiguity. The
  agent returns every healthy printer USB-first; a single-printer station resolves to exactly one
  `Device` and there is nothing to prompt about.

### Safari and the certificate

```bash
ls -l ~/Library/Application\ Support/browser-print-agentd/
openssl x509 -in ~/Library/Application\ Support/browser-print-agentd/cert.pem \
  -noout -dates -subject -ext subjectAltName
security find-certificate -c localhost -p /Library/Keychains/System.keychain | \
  openssl x509 -noout -fingerprint -sha1
curl -fsSk https://127.0.0.1:9101/available
```

- **No pair on disk** — `postinstall` did not get to generate it, or an uninstall removed it.
  Re-run the installer.
- **Pair on disk, `:9101` answers to `curl -k`, Safari still refuses** — the certificate is served
  but not trusted. Compare the SHA-1 fingerprints above; re-running the installer re-trusts it. On
  a correctly trusted station, `curl` validates `https://localhost:9101` with **no `-k` at all** —
  that is the sharper test.
- **Expired** — the certificate is valid 730 days and `postinstall` rolls it when a reinstall lands
  within 30 days of expiry. Re-running the installer is the renewal procedure.
- **The station's `/usr/bin/openssl` is LibreSSL.** The `postinstall` cert recipe drives OpenSSL
  through a config file for that reason: the `-addext` and `x509 -ext` forms do not exist in
  LibreSSL 3.3.6, which is what macOS ships. Do not "modernize" that recipe, and do not expect
  `-addext` to work when reproducing the cert by hand.
- Never ask an operator to trust a certificate. It is an admin action, and `postinstall` does it
  machine-wide under root precisely so nobody at the keyboard has to.

### The job left but no label

The agent's honesty guarantee ends at "CUPS accepted the job". Past that:

```bash
lpstat -o                                                # is the job sitting in the queue?
tail -50 ~/Library/Logs/browser-print-agentd/agent.log   # uid, byte count, lp request id, origin
```

Every job logs its device uid, byte count, `lp` request id, and origin, so a job in the log with no
matching `lpstat -o` entry reached the printer. Then it is the hardware: paused printer, head open,
media out, or a label that printed as a bitmap because something spooled without `-o raw`. A
printer that is not paused and still swallows jobs usually needs its one-time bring-up — SmartCal,
Label Top offset, print rate and darkness.

### A PDF label prints upside down

`POST /print-pdf` compensates for a driver that flips the page, and the compensation is applied
only to queues whose driver actually flips. If a rendered sheet comes out inverted, the question is
whether the agent recognised the driver:

```bash
lpoptions -p <queue> | tr ' ' '\n' | grep printer-make-and-model
```

CUPS's own label driver reports `Zebra ZPL Label Printer`. That driver's filter writes a literal
`^POI` — "invert 180" — into every job it renders, with no option or device default that can switch
it off, so the agent counter-rotates those jobs with `-o orientation-requested=6`. A queue with any
other driver is left alone on purpose: rotating one that does not flip would turn a correct page
upside down.

Three things make a sheet come out inverted anyway:

- **The queue reports a different driver.** A queue rebuilt against a vendor PPD or as IPP
  Everywhere does not go through that filter, so it should not be inverted in the first place — if
  it is, the flip is coming from somewhere else and `lpstat -l -p <queue>` will name the PPD.
- **`lpoptions` is missing or the queue is wedged.** An unanswerable driver probe is treated as
  "does not flip", which leaves the job unrotated. This direction is deliberate — the alternative
  is inverting every queue on the station on a guess — but it means a broken probe looks exactly
  like the original bug. Confirm `which lpoptions` resolves.
- **The verdict is cached for five minutes.** After changing a queue's driver, either wait it out
  or restart the agent with `launchctl kickstart -k gui/$(id -u)/io.github.sharaf-nassar.browser-print-agentd`.

`POST /write` is unaffected in all cases: ZPL is sent raw and whatever `^PO` command the caller put
in the label is what the printer obeys.

### Sleep, wake, and reboot

Nothing needs restarting by hand. The LaunchAgent has `RunAtLoad` and `KeepAlive`, so it comes back
after a reboot, a logout/login, and a crash; the health verdict is cached only ~3 s, so a printer
that reappears after wake is picked up almost immediately. If the station wakes and the caller
reports the agent unreachable, re-probe before touching anything — a single stale probe is not a
failure.

The one documented exception is a station whose login domain has gone into launchd's on-demand-only
mode — typically because a restart-pending macOS update is staged. There, none of `RunAtLoad`,
`KeepAlive` or `StartInterval` fires at all, so the agent neither survives a crash nor returns at
the next login. See
[The agent died and launchd never restarted it](#the-agent-died-and-launchd-never-restarted-it).
Keeping stations current on macOS updates is therefore an availability requirement, not
housekeeping.

### Known-good versions

The `lp -o raw` path was proven byte-exact on **macOS 26.4 / CUPS 2.3.4** against a **ZD621 300
dpi** over USB (device URI `usb://Zebra%20Technologies/ZTC%20ZD621-300dpi%20ZPL?serial=…`). The
agent has since been observed serving discovery and health on **macOS 26.6 (build 25G72)** on the
same hardware. macOS has removed raw queues and deprecated printer drivers, so treat a macOS
upgrade on a station as something to re-verify with the `lp -o raw` test above before a shift
depends on it.

### Quick reference

| Question                         | Command                                                                          |
| -------------------------------- | -------------------------------------------------------------------------------- |
| What version is running?         | `curl -fsS http://127.0.0.1:9100/health`                                         |
| Which queues does the agent see? | `curl -fsS http://127.0.0.1:9100/health` (shows unhealthy too)                   |
| What is the caller offered?      | `curl -fsS http://127.0.0.1:9100/available`                                      |
| Which agent is this?             | `curl -fsS http://127.0.0.1:9100/available` — read `provider`                    |
| Is the job registered?           | `launchctl print gui/$(id -u)/io.github.sharaf-nassar.browser-print-agentd`      |
| Why won't launchd start it?      | `launchctl print gui/$(id -u) \| grep 'on-demand count'`                         |
| Start it right now               | `launchctl kickstart gui/$(id -u)/io.github.sharaf-nassar.browser-print-agentd`  |
| What did the agent log?          | `~/Library/Logs/browser-print-agentd/agent.log`                                  |

`GET /health` is the one to reach for first: unlike `/available`, which hides unhealthy printers so
a caller can never pin one, it lists every discovered queue with its health, and it answers 200 even
when CUPS itself is unreachable — "alive at version X and blind to CUPS" is a diagnosis.

## Station validation checklist

Run this **once per station type**, on a real Mac with a real label printer, before any shift
depends on the agent. Nothing here can be proven by CI: automated coverage stops at stubbed CUPS,
and every item below is exactly the part a stub cannot see. Record the result of each item — pass,
fail, or observed value — against the release you validated.

**Setup:** a station Mac that still has Zebra Browser Print installed (so removal is actually
exercised), a Zebra printer on USB, a second Zebra reachable over the network for the failover and
multi-printer items, the calling page reachable, and the previous release's `.pkg` downloaded for
the last item.

A subset of the checklist does not need hands on the station: with SSH into the station account, an
unprivileged agent binary run straight from `/tmp` against the real CUPS queue settles everything
that is really about the agent talking to a real printer — discovery, the stable uid, real jobs on
real media, the version and CORS surface, the origin allowlist, the TLS listener, and the
honest-error path via `cupsdisable`/`cupsreject`. What is left over needs root, a GUI, a second
printer, a hand on the USB cable, or a power-state change.

### 1. Notarized install, Gatekeeper-clean

Install the release `.pkg` per [Installing on a station](#installing-on-a-station). Set the
quarantine bit by hand first if the download did not carry one, so Gatekeeper is genuinely
exercised rather than bypassed.

- [ ] `installer` succeeds with **no `xattr` quarantine removal and no right-click → Open**.
- [ ] `spctl --assess --type install -vv` reports `accepted` with `source=Notarized Developer ID`,
      and `stapler validate` passes, so the package installs offline.
- [ ] `preinstall` reports Zebra Browser Print removed; afterwards `/Applications`,
      `/Library/LaunchAgents`, `/Library/LaunchDaemons`, and `pkgutil --pkgs` hold nothing Zebra,
      and ports 9100/9101 are free before the payload lands.
- [ ] `postinstall` generates and trusts the cert, bootstraps the job, and its own `/available`
      probe passes, so the install exits 0 — with **no GUI authorization dialog and no
      administrator password prompt**, which is what makes an unattended install viable.
- [ ] `curl http://127.0.0.1:9100/health` reports the version you just installed, and
      `X-Print-Agent-Version` carries the same string.
- [ ] Repeat the install after a cold boot on an Apple-Silicon station that has never run this
      agent. Confirm no prior receipt, certificate, support directory, or launchd job exists,
      then verify the same install and first-launch results above.

> **Deferred:** the cold-boot first-install check requires an authorized, unused Apple-Silicon
> station and administrator access. No such station was available for this validation pass, so
> the successful upgrade and reinstall observations do not count as this proof.

### 2. Print a real label with Zebra absent

- [ ] From the calling page on the station, print a 4×1 label. A **physical label comes out**, and
      the on-screen feedback is unchanged from the Browser Print era.
- [ ] The agent log records the job with its device uid, byte count, `lp` request id, and origin.
- [ ] The same label prints over TLS on `:9101`.

### 3. USB-to-network failover

With both printers healthy and the USB one pinned by the caller:

- [ ] Pause or unplug the USB printer, then print. The job **fails over to the network printer**
      and prints there, the response is 200, and the agent log carries an explicit fallback line
      naming the skipped device.
- [ ] Now make **every** printer unusable and print again. The caller surfaces an **honest error**
      — never a phantom "Sent" — and `lpstat -o` shows no job was spooled. Prove this twice: once
      with the queue `cupsdisable`d and once with it `cupsenable`d but `cupsreject`ed, since the
      second case is the one only the `lpstat -a` probe catches.

> **Deferred:** the paused-USB-to-network direction requires an authorized Apple-Silicon station
> with a USB ZD621 and a second healthy network printer. No second printer or authorized
> queue-control session was available for this validation pass. Do not infer a pass from the
> automated failover tests: the required result remains a real label, HTTP 200, and the explicit
> fallback log line. Any phantom "Sent" is a hard failure.

### 4. Safari certificate path

On Safari, before the cert is trusted (uninstall and reinstall with the trust step skipped, or
temporarily remove the trust with `security remove-trusted-cert -d`):

- [ ] Opening `https://localhost:9101/available` in a tab shows a security warning.
- [ ] Re-run the installer to restore trust: the warning is gone, `curl` validates `:9101` with no
      `-k`, and printing resumes over `:9101`.
- [ ] A real label prints from Safari.
- [ ] On Chrome, the same station prints over plain `http://localhost:9100` with **no cert step at
      any point**.

### 5. Multi-printer resolution

With two USB Zebras attached to one station:

- [ ] `/available` returns both, USB-first, and the caller's chosen `uid` persists across a reload.
- [ ] Detach one. `/available` returns exactly one `Device` and `/default` resolves it, so a
      single-printer station has nothing to prompt about.
- [ ] With one USB and two network printers, the USB device still comes first.

### 6. Sleep, wake, logout, reboot, crash

- [ ] Sleep the Mac, wake it, print. It prints with no manual restart.
- [ ] Change the station's active network, wait for CUPS to rediscover its network queues, then
      print. `/available` recovers and the next job prints with no agent restart.
- [ ] Log out and back in; the agent is running again (`RunAtLoad`).
- [ ] Reboot; same.
- [ ] `kill -9` the agent process; `KeepAlive` brings it back within ~10 s and printing works.

> **Deferred:** sleep/wake and network-change recovery require an authorized operator to change
> station power and network state while a real USB ZD621 is available. This validation pass had
> neither an authorized station session nor permission for those disruptive transitions, so the
> existing reboot and crash-recovery observations do not close these checks.

If the `kill -9` bullet fails with `pended nondemand spawn`, do not go looking for a plist fix —
that is the launchd on-demand gate, and
[The agent died and launchd never restarted it](#the-agent-died-and-launchd-never-restarted-it)
carries the diagnosis and the remediation. It also suppresses `RunAtLoad`, so it blocks the logout
and reboot bullets behind the same fix.

### 7. Big job — ~540 KB 4×6 `^GFA`

- [ ] Print a real 4×6 label. The full uncompressed `^GFA` payload (~540 KB) reaches the printer
      **without truncation**, and the label renders as artwork rather than as a garbled or partial
      image.
- [ ] The byte count in the agent log matches the payload the caller sent, byte for byte, and the
      queue drains with no truncation error.

### 8. Unplug/replug keeps the same stable uid

This validates the stable-uid choice against real USB re-enumeration, which a stub cannot
reproduce.

- [ ] Record the printer's `uid` from `/available` and confirm the caller has pinned it.
- [ ] Unplug the USB printer, wait for it to leave `/available`, replug it, wait for it to come
      back.
- [ ] `/available` reports **the same `uid` as before**. The caller's pinned `uid` still matches:
      **no re-prompt, no orphaned pin**, and the next print goes straight to that printer.

The uid is a hash of the raw device URI, so it is stable as long as the URI is. If the observed uid
changed, capture both `lpstat -v` outputs — the URI itself changed across re-enumeration, and that
is a design-level finding, not a station problem.

> **Deferred:** this check requires physical access and authorization to unplug a real USB ZD621
> from an Apple-Silicon station. Neither was available for this validation pass. The uid remains
> unproven across real re-enumeration; if it changes, record the before/after URI behavior as a
> `stableUID` design finding rather than treating it as a station quirk.

### 9. Wedged USB device still answers inside 1500 ms

This validates the bounded-health mitigation: each `lpstat` probe is capped at 900 ms and probes
run concurrently, specifically so a hung device cannot blow a caller's 1500 ms probe abort.

Wedge the printer — powered on but not responding (media jam with the head open, or a firmware
hang) so that `lpstat -p <queue>` visibly stalls.

- [ ] `time curl -fsS http://127.0.0.1:9100/available` returns in **well under 1500 ms**. The
      healthy-path baseline to compare against is 0.10–0.12 s.
- [ ] The wedged printer is **absent from `/available`** and present in `/health` marked unhealthy.
- [ ] The caller still reports the agent reachable and still offers any other healthy printer.

### 10. Uninstall

- [ ] `sudo browser-print-agentd-uninstall` completes, and every check in
      [Uninstalling](#uninstalling) comes back clean — no job, no receipt, no trusted cert, ports
      free.
- [ ] Reinstalling afterwards produces a working station again.

### 11. Downgrade — reinstall the prior `.pkg`

Run this **once**, ever, to validate the rollback path itself; after that,
[Rolling back](#rolling-back-to-the-previous-release) is a documented operation rather than an
experiment. It needs two notarized `.pkg`s that were actually published under `vX.Y.Z` tags.

- [ ] Install the previous release's `.pkg` over the current one, with **no uninstall first**.
- [ ] `installer` succeeds: no downgrade guard trips, and `preinstall` boots out the newer running
      agent rather than being blocked by it.
- [ ] `/health` reports the **older** version and `/available` answers with the station's printers.
- [ ] A real label prints.
- [ ] The station cert was **reused, not regenerated** — the SHA-1 fingerprint in the System
      keychain is unchanged and Safari still trusts `https://localhost:9101` with no re-prompt.

Record the observed result under [Rolling back](#rolling-back-to-the-previous-release), replacing
its "not yet hardware-validated" note.

### 12. A rendered PDF sheet prints the same way up as a pinned ZPL label

This is the one item that proves the driver-orientation compensation, and it cannot be proven by
CI: automated coverage stops at the `lp` argv, while whether the page lands upright is a property
of the filter chain and the physical printer. Use content with an unambiguous top — a solid bar
across the top quarter is enough.

- [ ] `lpoptions -p <queue> | tr ' ' '\n' | grep printer-make-and-model` reports the driver, and
      the station's Zebra queue reports `Zebra ZPL Label Printer`.
- [ ] `POST /write` a ZPL label that pins its own orientation. Record which way up it comes out.
- [ ] `POST /print-pdf` the same content rendered as a PDF. It comes out **the same way up**, not
      inverted.
- [ ] The agent log shows both jobs going to the same queue — a fallback to a different printer
      changes which driver applied and invalidates the comparison.
- [ ] On a second queue whose driver is NOT `Zebra ZPL Label Printer` (add one temporarily if the
      station has none), a `/print-pdf` sheet is **not** inverted either. This is the half that
      catches an over-eager compensation, and it is the failure a station would otherwise discover
      only after a shift printed upside down.
