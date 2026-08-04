# Spec: manual-update-check

> **Status: NOT BUILT — superseded at the clarification gate.** The human answered Critical
> Question 1 as *latency, not agency*: the interval moved from 86400 to 3600 seconds and the
> load-time run was fixed to actually happen, which removed the staleness that motivated a manual
> trigger. CQ2-CQ7 are moot. This document is retained because its review findings remain true of
> any future attempt at a user-facing trigger — see `## Clarifications` at the end for what shipped
> instead, and `lat.md/operations#Station Operations#Auto-Update Decision#Rejected Alternatives`
> for the durable record.

Draft. Breadth over polish — uncertainty is recorded rather than resolved.

## Problem Statement

A station runs the auto-updater on an 86400-second `StartInterval` with up to 900 seconds of
jitter. Between one run and the next, a station that is known to be behind — because a fix just
shipped, or because `/health` already says so — has no way to catch up except waiting up to a day
or having an administrator run a `launchctl` command over SSH or Terminal.

The people standing at these stations are not administrators and do not use a terminal. Today the
only manual path is `sudo launchctl kickstart -k system/<updater-label>`, which needs both a
terminal and an admin password. That is the gap: **a station user must be able to ask for an update
check on demand, from the GUI, without a password, without a terminal, and without the agent
gaining any new attack surface.**

Why now: `/health` already exposes update status (`lat.md/tools#Print Agent#Version And Health
Surface`), so a station can already *tell* you it is behind. Only the ability to *act* on that is
missing, which makes the existing visibility frustrating rather than useful.

## Goals

1. A station user with no admin rights can trigger an update check from the macOS GUI, in one
   double-click, with no password prompt and no terminal window.
2. A manually triggered check begins its network request within a few seconds — it must not
   inherit the up-to-900-second jitter sleep. Target: user-visible outcome within 60 seconds on a
   normal connection.
3. The user gets a legible outcome back — at minimum one of: already up to date / update installed
   / update failed / check already in progress / updates pinned by an administrator.
4. Zero change to the agent: no new route, no outbound network path, no change to the frozen wire
   contract, no field added to `Device`, no change to `X-Print-Agent-Version`.
5. Zero change to the verification chain. A manual check reuses the existing manifest fetch, sha256
   gate, Team ID pin, quarantine bit, `spctl` notarization assessment, health probe, rollback, and
   failed-version quarantine, byte for byte.
6. The administrator pin still works and is still one command. A pinned station must not check or
   install when a user clicks, and must say so rather than failing silently.
7. `scripts/check-naming.sh` stays green; every new name derives from `packaging/identity.sh`.

## Non-Goals

- **No update UI in the browser page.** No new agent route, no button that a web page can reach.
  This is the exposure `lat.md/operations#Station Operations#Auto-Update Decision#Rejected
  Alternatives` already rules out: an unauthenticated loopback service that any open tab can use to
  drive a root install run.
- **No menu bar app, no background GUI agent, no Sparkle.** The product deliberately ships no
  `.app` for the agent itself, and Sparkle cannot do unattended `.pkg` installs.
- **No progress bar, no update history window, no preferences UI.** One dialog with one outcome.
- **No change to the update policy.** Manual check does not bypass the manifest, does not allow
  choosing a version, does not allow downgrade selection, and does not re-try a quarantined failed
  version.
- **No new agent tests.** Per `CLAUDE.md`, new tests require an explicit request; this spec does not
  assume one. Verification is by station validation steps unless the human asks otherwise.
- **Not a replacement for the daily run.** The scheduled updater remains the primary mechanism.

## Backlog Inputs

None. `bd list --status=open` returned an empty set at the start of this run; there are no P4
sources, no `source_backlog`, and no provenance closure to compute.

## Target Epic

None supplied and none inferable — there was no `epic` var, no `epic_candidates`, and no existing
open issue to walk. **This run will create the feature epic.**

## User Stories

### Story 1 — A station user asks for an update

As a **station user with no admin rights**, I want to ask the station to check for an update right
now, so that I can pick up a fix without waiting up to a day or finding an administrator.

Acceptance Criteria:
- A clearly named item exists in `/Applications` (title derived from `PRODUCT_TITLE`).
- Double-clicking it triggers a real update check. No password prompt. No Terminal window appears.
- It works when logged in as a standard (non-admin) account.
- Within 60 seconds the user sees a dialog naming the outcome.
- Triggering it when already up to date leaves the station untouched and says so.

### Story 2 — A manual check is not silently swallowed

As a **station user**, I want a click to either do something or tell me why it did not, so that I
do not click repeatedly believing nothing is happening.

Acceptance Criteria:
- If a check is already running (including one asleep in its jitter window), the user is told a
  check is in progress rather than getting silence or a false "up to date".
- If the updater is pinned by an administrator, the user is told the station is pinned. No check
  runs.
- If no console user resolves, or the status file cannot be read, the dialog reports an
  indeterminate result rather than fabricating success.
- The dialog never reports success on the basis of a stale status file written by an earlier run.

### Story 3 — The administrator pin still holds

As an **administrator**, I want the one-command pin to keep a station on its current build, so that
a station I deliberately held back cannot be moved forward by a user clicking a button.

Acceptance Criteria:
- After the documented pin command, a manual trigger installs nothing.
- `/health` continues to report `pinned: true` from launchd's live disabled override, unchanged in
  shape.
- The pin remains **one** command. If the chosen design introduces a second launchd label, the
  RUNBOOK command and `/health`'s pin derivation must both be updated to cover it, and that is a
  cost counted against that design.

### Story 4 — Nothing about the agent changes

As a **maintainer**, I want the print agent untouched, so that the frozen wire contract and the
zero-egress posture survive this feature.

Acceptance Criteria:
- No diff to `server.go`, `discovery.go`, `cups.go`, `zpl_document.go`, or the `Device` shape.
- `go build ./...`, `go vet ./...`, `go test -race ./...` green with no test changes required.
- `scripts/check-naming.sh` green.
- `packaging/build-pkg.sh --stage-only` shows the new payload in the expected layout.
- `lat check` green; `lat.md/operations.md` and `lat.md/packaging.md` updated.

### Story 5 — The installer still passes Gatekeeper

As a **maintainer**, I want the release to still notarize and staple, so that stations install
clean and the updater's own `spctl --assess --type install` gate keeps passing.

Acceptance Criteria:
- Every executable added to the payload is signed with `APP_SIGNING_IDENTITY` when one is supplied.
- Notarization of the resulting `.pkg` succeeds.
- On a station, `spctl --assess --type install` on the downloaded package still reports Notarized
  Developer ID.
- Uninstall removes every artifact this feature adds, leaving no launchd record and no orphan in
  `/Applications`.

## Constraints

**Frozen surfaces.** The wire contract (`/available`, `/default`, `/write`, `/read`, `/health`,
their shapes, status codes, plain-text error bodies, CORS origin echo, the 1500 ms `/available`
probe budget, byte-exact `^GFA` passthrough) does not change. `/default` still returns an empty
body. `X-Print-Agent-Version` is a fixed header name and `Device` never grows a version field.

**Identity.** Every new path, label, and name lives in `packaging/identity.sh` and nowhere else.
Every packaging artifact is a `.in` template rendered by `build-pkg.sh`. No literal bundle id,
label, binary name, or install path anywhere else.

**Privilege split.** Only the root updater daemon has network egress or install privilege. The
agent stays an unprivileged per-user LaunchAgent. Nothing this feature adds may run as root on
behalf of a user except via launchd's own scheduling.

**No credentials, no Team ID, no lab strings** in any file, log, command line, or commit.
`scripts/check-naming.sh` has no allowlist.

**Existing mechanism to reuse, unmodified:** `packaging/updater.sh.in` already does manifest fetch,
`pkgutil --pkg-info` comparison, sha256 gate, `pkgutil --check-signature` Team ID pin (trust on
first install), explicit `com.apple.quarantine` set plus `spctl --assess --type install`,
`installer -pkg … -target /`, `/health` version probe, rollback to the cached verified previous
package, failed-version quarantine, and the sanitized public status file at
`UPDATE_STATUS_PATH`. `/health` derives `pinned` live from `launchctl print-disabled system`
(`RUNBOOK.md:373-378`), never from the updater's own file write.

**Relevant existing code points:** jitter sleep at `packaging/updater.sh.in:363-367`; console-user
guard at `:354-356`; `pgrep -x installer` guard at `:349`; public status file write at `:104-116`;
binary codesign at `packaging/build-pkg.sh:310-315`; updater bootstrap that honors the disabled
override at `packaging/scripts/postinstall.in:320-326`; updater teardown at
`packaging/uninstall.sh.in:125-132`.

## Open Questions

**Q1 (blocking, for the clarify gate) — one launchd label or two?**

The incoming design assumed a second label (`UPDATER_LABEL.manual`) with `WatchPaths`. Drafting
this spec surfaced a cheaper variant that appears strictly better, and the choice changes several
downstream stories:

- **Variant A — second label.** New `packaging/updater-manual.plist.in`, label
  `<updater-label>.manual`, `WatchPaths` on the trigger, `ProgramArguments = [updater, --now]`.
  Costs: a `mkdir` lock in the mode-700 state dir (two labels *can* run concurrently and the
  `pgrep -x installer` guard alone does not prevent two concurrent downloads); the pin becomes two
  commands unless `/health` and the RUNBOOK are reworked; postinstall must register a second job;
  uninstall must tear down a second job. Benefit: a manual check can still run when the scheduled
  job is stuck mid-download.
- **Variant B — one label, `WatchPaths` added to the existing `updater.plist.in`.** launchd will
  not run two copies of the same label, which gives the concurrency lock for free — no `mkdir`
  lock. The pin stays exactly one command, `/health`'s `pinned` derivation is unchanged, postinstall
  and uninstall are unchanged. The script discriminates manual from scheduled deterministically by
  comparing the trigger file's mtime against a last-seen value recorded in the mode-700 state dir —
  no timing heuristic. Its one real failure mode: a touch that arrives while the job is already
  running (typically asleep in its jitter window) does not start a second run and the event is
  coalesced away. Mitigated by having the jitter sleep poll the trigger mtime and break early, which
  converts that failure into the desired behaviour.

Variant B is the recommendation: smaller diff, fewer moving parts, and it preserves the
one-command pin that Story 3 asks for. **The clarify gate must pick one.**

**Q2 — does `WatchPaths` actually fire on a bare `touch`?** Expected yes (kqueue `NOTE_ATTRIB`
from `utimes`), and it is the common idiom, but this is load-bearing and unverified on a real
station. Also unverified: whether launchd coalesces or thrashes on repeated writes, and its
behaviour when the watched path exists at load time versus is created later. Needs a station test
before the design is committed to.

**Q3 — is a mode-666 root-owned trigger file acceptable?** The dir above it is root-owned 755, so
an unprivileged user can change the file's mtime but cannot delete, replace, or symlink it, and the
updater never reads its contents. The residual capability is "any local user can cause an update
check", bounded by the fact that installs remain gated on sha256, Developer ID, and notarization.
Confirm this is an accepted posture, and confirm it does not violate the "root-owned mode 755 solely
to provide traversal to one sanitized mode-644 status file" description in
`lat.md/packaging#Packaging#Station Installer#Root Updater LaunchDaemon` — that documented invariant
will need rewording either way.

**Q4 — will a script-only `.app` bundle notarize?** Notarization requires signed executables;
hardened runtime applies to Mach-O binaries, of which a shell-script app bundle has none.
Expectation is that a Developer ID signature over the bundle suffices, but `build-pkg.sh:310-315`
currently signs only the agent binary, and this has never been exercised. If it fails, the fallback
is a small Go binary as the bundle executable — same signing path as the agent, larger diff.

**Q5 — how does the app read the outcome?** The public status file is sanitized and bounded, but the
app must not report success from a stale write. Proposal: record the status file's mtime before
touching the trigger, then wait for it to change, with a hard cap. Open: what the cap should be (60 s
proposed), and what a timeout should say — an install can legitimately outlast any reasonable dialog
timeout, so "still working, check `/health` later" may be the honest message.

**Q6 — where does the pinned state come from for the dialog?** `/health` exposes `pinned` and the
agent is on loopback, so the app could read it there without needing privilege. Alternative: the app
reads `launchctl print-disabled system` directly (unprivileged read). Undecided which is the smaller
dependency.

**Q7 — what exactly is the `/Applications` item named?** It must derive from `PRODUCT_TITLE` and
survive `scripts/check-naming.sh`. A name containing an em dash or the word "Update" may read badly
next to unrelated software on a shared station.

**Q8 — does the console-user guard interact badly with this?** The updater skips entirely when no
console user is present (`packaging/updater.sh.in:354-356`). A manual trigger implies a console
user, so this should be inert, but the interaction with fast user switching and the screen-locked
state is unverified.

**Q9 — does RUNBOOK's station validation checklist grow an item?** `lat.md/operations#Station
Operations#Station Validation Checklist` has eleven items covering exactly where automated coverage
stops. A GUI trigger on a real station is precisely that kind of thing.

## Spec Review

Six parallel review passes (requirements, gaps, ambiguity, feasibility, scope, stakeholders) against
the draft above. Findings that landed in three or more dimensions independently are treated as
high-confidence and are promoted to Critical Questions. Several of them invalidate parts of the
incoming design rather than merely refining it.

### Critical Questions (answer before planning)

**CQ1 — Is the actual requirement latency, or user agency?** — *flagged by: scope*

The Problem Statement argues **latency** ("wait up to a day"). Goals 1-3 pre-commit to a GUI with an
outcome dialog, which is **agency**. These are different products with roughly a 40x difference in
diff size. If the requirement is latency, it is met by two integers: `StartInterval` 86400 → 3600 in
`packaging/updater.plist.in`, and the jitter modulo 901 → ~301 at `packaging/updater.sh.in:365`.
Worst-case staleness drops from ~24h15m to ~1h05m. The incremental cost is one ~512-byte HTTPS GET
per station per hour — the run exits at `write_status current` (`updater.sh.in:399-403`) before any
package download. Zero new artifacts, zero signing or notarization risk, pin and `/health`
untouched, no `.app`, and CQ2 through CQ7 all evaporate.

Everything below only matters under the agency reading. **This question gates the other six.**

**CQ2 — Does a user-initiated update get to interrupt a live shift, with no confirmation?** —
*flagged by: gaps, stakeholders*

A successful manual check is not a check — it is a full package install. `preinstall.in:128,169-183,
222-228` boots out the LaunchAgent, kills port holders, and waits for 9100/9101 to drain;
`postinstall.in:251-256,262-298` re-bootstraps and re-probes. One double-click therefore stops
printing for tens of seconds and drops any in-flight `/write`. The scheduled run's jitter made that
collision unlikely by accident; a button makes it happen at the worst possible moment by design,
because the user most likely to click is the one currently having trouble printing.

The draft has no confirmation step, no in-flight-job check, and no "printing will pause" warning —
and Goal 1 actively forbids a prompt. Decide: does the button check-and-report only (deferring the
install to the next scheduled run), install with an explicit confirmation, or install silently?
Related: an administrator who wants the daily cadence but not user-initiated installs during shift
hours currently has only the all-or-nothing pin.

**CQ3 — Does the world-writable trigger survive its own threat model, and where does it live?** —
*flagged by: scope, feasibility, ambiguity, gaps, stakeholders*

Three separate problems in one mechanism:

1. **It reproduces the capability used to reject the browser route.** Non-Goals rejects an agent
   route because "any open tab can drive a root install run". A mode-666 trigger grants *any local
   process* — including a browser subprocess — the same capability with *less* auditability than an
   HTTP route would have. Either the threat model tolerates local-actor-initiated update runs (in
   which case the recorded rejection rationale in `lat.md` needs rewriting, and the browser route
   deserves reconsideration on its merits) or it does not (in which case this design is dead). It
   cannot be both.
2. **Mode 666 is not mtime-only.** Any local user can open it `O_WRONLY` and write unbounded bytes
   into `/Library/Application Support/` — a boot-volume-fill vector. Mode 622, a trigger directory,
   or a size check in the updater each close it.
3. **Its proposed location may cause a self-retrigger loop.** `write_status` publishes by `mktemp` +
   `mv -f` into `$STATE_ROOT` (`updater.sh.in:107,116`) — the same directory the trigger would live
   in. launchd watches the nearest existing ancestor when a watched path is absent, and a directory
   watch fires on any rename inside it. If the trigger is ever missing (fresh install before
   postinstall creates it, admin deletion, rollback), every status publication re-triggers the job:
   an unbounded root update loop.

Also unresolved and cross-cutting: no rate limit, cooldown, or minimum interval anywhere in the
draft, while each successful install bounces the print agent.

**CQ4 — The feature is inert on every existing station until reboot. Which variant, and shipped
how?** — *flagged by: gaps, feasibility*

`postinstall.in:322-324` deliberately does nothing when the updater label is already registered,
because booting it out would kill the `installer` process the running updater invoked
(`lat.md/packaging.md:190-194`; this is the documented v0.3.0/v0.3.1 interval divergence). Under
Q1's recommended **Variant B**, the new `WatchPaths` key lands on disk while the *loaded* job keeps
the old definition. The `.app` installs and is clickable immediately, the trigger is touched,
nothing spawns, no status file changes, and the dialog can only time out — on the exact release that
ships the feature. Self-heal is not available: a script that re-bootstraps its own label kills
itself mid-run.

This materially re-decides Q1, since **Variant A**'s new label *is* bootstrapped by postinstall
normally. Options: ship the plist change in release N and the `.app` in N+1; have the `.app` detect
the missing `WatchPaths` in the loaded job and say "restart this Mac once"; document reboot-required
and accept a broken first click; or take Variant A and pay for the `mkdir` lock, the second pin
command, and the `/health` pin-derivation change.

**CQ5 — Pin state, in-progress state, and outcome reporting have no data source. Resolve as one
decision or cut the dialog.** — *flagged by: requirements, ambiguity, feasibility, stakeholders*

Q5 and Q6 are not two questions. Under the mtime-watching design:

- **Pinned stations never write a status file** — a disabled label never runs — so "pinned" can only
  ever surface as a timeout, not as an answer. It must be read *before* the touch.
- **Worse, the pin may not hold at all.** `lat.md/packaging.md:170-171` records that a watch-path
  event is a *demand-reason* spawn, which escapes a launchd gate that other spawn reasons do not.
  Whether `launchctl disable` is consulted on a demand start is untested here. **If it is not,
  Story 3 fails outright and a user click updates a deliberately held-back station.** This is the
  cheapest test on the list and should run first.
- **"A check is in progress" has no source.** Nothing writes a "started" record; `STATE_DIR` is
  mode 700 so the unprivileged app can read nothing private; the public file is written only at
  terminal states. `pgrep -x updater` will not match either — the job execs `/bin/sh <path>/updater`,
  so `comm` is `sh`, unlike the deliberate `pgrep -x installer` at `:349`.
- **At least four exit paths write no status at all**: `early_fail` at `:51` (not root) and `:57`
  (symlinked dir), and the `fail` calls at `:373-375` and `:377-379`. A hard failure is
  indistinguishable from "still working".

Story 2's first acceptance criterion also contradicts both Q1 variants: it says a click during the
jitter window reports "in progress", while both variants are explicitly designed to make that click
*run*. Note the mechanism itself is sound where it exists — `write_status` always `mv -f`s a fresh
inode so mtime genuinely changes, and the file is mode 644 under a 755 parent so an unprivileged
reader works. The gap is coverage, not plumbing.

**CQ6 — What does the dialog actually say? Fourteen real statuses, five spec outcomes, and a 60-second
promise that cannot be kept.** — *flagged by: requirements, gaps, ambiguity, feasibility, stakeholders*

`updater.sh.in` writes fourteen distinct status tokens, all allowlisted in `update.go`: `current`,
`updated`, `quarantined`, `rolled-back`, `rollback-failed`, `rollback-cache-failed`,
`checksum-failed`, `trust-failed`, `manifest-fetch-failed`, `manifest-invalid`,
`package-fetch-failed`, `install-failed`, `skipped-install`, `skipped-no-user`. Goal 3 names five
outcomes and hedges with "at minimum", Story 2 adds a sixth ("indeterminate", undefined), Q5 adds a
seventh. No mapping exists. Concretely: a station sitting on a `quarantined` version — one that
already failed once — will report whichever generic branch it falls into, forever, to a user whose
`/health` says they are behind. And `skipped-install` fires on *any* unrelated macOS install running
concurrently, an outcome the draft does not contemplate.

The 60-second bound is stated three ways with three different obligation strengths (Goal 2 "target",
Story 1 acceptance criterion "within 60 seconds", Q5 "what should the cap be"), and Q5 already
concedes it is unreachable. Verified: removing jitter does not shorten the work. A cold rollback
cache downloads the *currently installed* package first (`:258-273`, `--max-time 900` at `:140-144`),
then the new one, then installs, then waits up to 30 s for the health probe (`:293-312`). 60 s is
achievable only for `current`, `quarantined`, `skipped-*`, and the fetch/parse failures.

Needed: the full token → dialog-text map, and Goal 2 restated as "the check *starts* within seconds"
plus an explicit two-phase or fire-and-forget dialog contract.

**CQ7 — Pre-decide the `.app`'s executable form and its packaging consequences.** — *flagged by:
gaps, feasibility, scope, stakeholders*

This is the payload's first bundle and the product's first permanent GUI surface. Four consequences
the draft does not count:

- **`pkgbuild` will make it relocatable.** `build-pkg.sh:328-337` runs `pkgbuild --root … --install-
  location /` with no `--component-plist`; the synthesized component defaults
  `BundleIsRelocatable=true`, so `installer` may retarget the bundle to a copy found elsewhere on
  disk, leaving `/Applications` empty and the receipt pointing somewhere undocumented. The app's
  `CFBundleIdentifier` is also unspecified, while `BUNDLE_ID` is already both the launchd label and
  the productbuild package identifier.
- **Notarization is unproven and its fallback is not a "larger diff".** A bundle whose
  `Contents/MacOS` entry is a shell script has no Mach-O; `--options runtime` records a flag nothing
  enforces, and the host process is Apple's `/bin/sh`. The fallback — a second Go binary — needs a
  new package directory, contradicting the flat-`package main` invariant in `CLAUDE.md`, plus a
  second cross-compile target, a second codesign step, and a second `identity.sh` ↔ `identity.go`
  assertion in `check-naming.sh`. **Decide the abandon-vs-accept branch now**, not after a failed
  `notarytool` submission.
- **Rollback leaves an orphan.** Reinstalling the previous `.pkg` does not delete payload files
  absent from the older package, so a rolled-back station keeps a clickable app wired to a trigger
  the older updater ignores. `uninstall.sh.in:200-205` also never reaches `/Applications`.
- **Non-Goals does not hold the surface line.** It forbids growth *of this feature*, not *of the
  bundle*. The status file already carries `latest-version`, making a version display a nearly-free
  ask within a week; then "open the log", "restart the agent", a Login Item auto-check, Notification
  Center. An explicit statement that the bundle performs exactly one action for its lifetime, and
  who declines the rest, is needed.

### Non-Blocking Observations

- **Q1 is arguably already answered in its own text** — but CQ4 reopens it on new evidence, so it
  must be re-decided rather than confirmed. Q2, Q4 and Q8 are the only genuine unknowns; Q3, Q5, Q6
  and Q7 are decisions. Q9 is a yes.
- **Variant B's jitter-poll rework serves a ~1% window.** Converting `/bin/sleep "$jitter"` into a
  polling loop makes the daily job's timing code load-bearing for a GUI feature on stations where
  nobody ever clicks, to catch a click landing inside a 900 s sleep out of 86400 s. Telling the user
  "a check is already running" is the correct free answer — if CQ5 can source it.
- **`Constraints` says `updater.sh.in` is reused "unmodified" and Goal 5 says "byte for byte"**,
  which as written forbids both Q1 variants. Reword to scope the freeze to the *verification* steps.
- **`update.log` has no rotation.** `updater.sh.in:23-70` appends unbounded to
  `/Library/Logs/<product>/update.log`; the bounded ring in `lat.md/packaging#Request Log Ownership
  And Rotation` covers only the *agent* log. Manual triggering changes its growth model from
  one entry per day to user-driven.
- **`update.log` is mode 600 root:wheel** (`:69`), so a user shown "update failed" can read nothing
  and cannot be talked through anything by phone. No RUNBOOK "dialog said X → do Y" entry exists.
- **No manual-vs-scheduled marker anywhere.** The public status file's grammar is fixed at four
  records and `update.go` hard-rejects any other shape, so attribution on a shared station is
  structurally impossible. Diagnosing "the user clicked and nothing happened" has no artifact.
- **Multi-user stations are undefined.** The console-user guard (`:354-356`) checks that *a* console
  user exists, not that the triggering user is that user — a switched-away account can bounce the
  print agent out from under whoever is printing. Two accounts clicking share one root-owned status
  file with no correlation token.
- **`ThrottleInterval` defaults to 10 s**, so rapid re-clicks coalesce. Acceptable, but the dialog
  copy must not promise otherwise.
- **The bare-`touch` assumption is the safest of the launchd unknowns.** `utimes(path, NULL)`
  succeeds for a non-owner with write permission and raises `NOTE_ATTRIB`; what needs station
  verification is that modern launchd registers `NOTE_ATTRIB` rather than write-only flags. Note
  `StartInterval` and `WatchPaths` coexisting is *not* a concern — independent start conditions.
- **Packaging surface is larger than one file.** An `.app` is several payload files (`Info.plist`,
  `Contents/MacOS/<x>`, `PkgInfo`), each needing its own `.in` and a `render_template` call, because
  `build-pkg.sh:277-283` fails the build on any unrendered placeholder anywhere in the tree.
  `:305-308` lints only the two existing plists, so an `Info.plist` lint must be added.
- **The app name must be checked against `preinstall.in:187-199`**, which deletes
  `/Applications/Zebra*Browser*Print*` and `/usr/local/bin/*[Bb]rowser[Pp]rint*`. A
  `Browser Print Agent*.app` is not self-deleted today, but the globs are one character away from
  removing the installer's own new payload. `check-naming.sh` itself cannot fail on any
  `PRODUCT_TITLE`-derived name.
- **`/health`-as-pin-source fails exactly when it is needed.** Q6 should default to
  `launchctl print-disabled system` (already run unprivileged from `update.go:195`); reading
  `/health` makes the GUI a second consumer of the frozen wire contract and returns nothing when the
  agent is down, which is when someone clicks.
- **Availability expectation changes for the calling page.** Story 4's "nothing about the agent
  changes" is true of the API and false of uptime: the page now faces connection-refused at a moment
  chosen by the station user rather than by the schedule.
- **Verification ownership is unstated.** Non-Goals settles on station validation; Q9 asks whether
  the checklist grows an item; nobody is named as signing off that the feature works before release.
- **Unmeasurable acceptance vocabulary**: "legible outcome", "clearly named", "expected layout",
  "normal connection", "a few seconds", "reads badly". None can fail a review as phrased.
- **Missing explicit out-of-scope statements**: no second compiled artifact; no Login Item or
  launch-at-login; no localization (dialog is English-only); the bundle never becomes the agent's
  launcher; no self-update of the bundle independent of the `.pkg`; no telemetry; no remote/MDM
  trigger path; no change to the scheduled run's status-file semantics.
- **Backlog Inputs "None" is confirmed consistent** — `bd list --status=open` is empty, and
  Target Epic correctly declares this run creates the epic.

## Clarifications

**CQ1: Is the actual requirement latency, or user agency?**

A: **Latency.** Before answering, the human asked whether a push mechanism could replace polling
outright. It cannot: device-level APNs needs MDM enrollment these unmanaged stations do not have,
and the updater is a shell-script LaunchDaemon with no bundle or push entitlement. A self-hosted
socket would work but would introduce this product's first service backend as a new single point of
failure, end the short-lived process model that makes the self-update race impossible, restore
`KeepAlive`, and stand a persistent outbound root connection on every station — and would still
need a reconciliation poll underneath it. The full reasoning is recorded in
`lat.md/operations#Station Operations#Auto-Update Decision#Rejected Alternatives`.

The accepted answer is a shorter interval plus a working load-time check:

- `StartInterval` 86400 → **3600** (`packaging/updater.plist.in`).
- Jitter modulo 901 → **301**, so the 0-300 s spread stays well inside the hourly interval
  (`packaging/updater.sh.in`).
- The console-user guard now **waits up to 300 seconds** for a login instead of testing once.
  This is the load-bearing part: `RunAtLoad` was already set, but launchd starts the job at boot
  while `/dev/console` still belongs to `root`, so every boot run exited immediately as
  `skipped-no-user`. Startup checks did not previously happen at all.

Worst-case staleness drops from ~24h15m to ~1h05m, and a reboot now reliably forces a check.

**Explicitly not delivered — logging out and back in does not trigger a check.** The updater is a
system-domain LaunchDaemon and is not reloaded by a user session; only a reboot reloads it. Making
a relog trigger a check would require the per-user LaunchAgent to poke the daemon, which is the
`WatchPaths` design this spec's review already found unsafe (CQ5: `launchctl disable` may not
survive a demand-start, which would break the administrator pin).

**CQ2-CQ7: moot.** No `.app` bundle, no trigger file, no second launchd label, no dialog, no
packaging or notarization change. The concerns they raise are preserved above for any future
attempt.

**Conditional GET (`--etag-save`/`--etag-compare`) was considered and skipped.** At 24 checks per
day against a ~120-byte static asset it is not worth the state file; revisit only if the interval
goes below ~15 minutes.

**Backlog Inputs:** none — nothing to refine, supersede, or retire.

**Target Epic:** none created. The delivered change is two integers and one bounded wait loop,
already implemented and validated in this run; a feature epic with task beads would have been
scaffolding around a diff smaller than its own tracking.
