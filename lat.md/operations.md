# Station Operations

What an administrator does to a station Mac once the package exists: install it, migrate onto it,
roll it back, take it off, and find out why it stopped printing. `RUNBOOK.md` is the document that
owns all of it.

The split with `README.md` is deliberate and the runbook states it in its own opening: `README.md`
says what the agent **is** — the frozen wire contract, the install layout, the release chain — and
`RUNBOOK.md` says what you **do** with it. This graph is the third register. It records why the
product behaves the way it does, so a runbook procedure earns a section here only when the reason
behind it is a design decision rather than a keystroke.

Six chapters carry the whole station lifecycle: where installers live and why every one of them
stays downloadable, installing, migrating from another localhost print agent, rolling back,
uninstalling, and a layered diagnostics chapter that always opens with `GET /health`. Diagnostics
is the operational face of [[tools#Print Agent#Version And Health Surface]] and
[[tools#Print Agent#Health And Failover]] rather than a second source of truth for either — it
tells an admin which layer to rule out next, and every verdict it reads is one the agent already
computes. Its one station-side remediation is the launchd on-demand gate: kickstart to print now,
then install the pending macOS update and reboot, because
[[packaging#Packaging#Station Installer#The launchd On-Demand Gate|no plist change clears it]].

Three things are deliberately out of scope. There is no build-from-source or side-load path,
because the agent ships only as a signed, notarized `.pkg` attached to a `vX.Y.Z` release and
nothing on a station auto-updates — every version change is an admin installing a specific package
on purpose, which is what makes the rollback below one command. Nothing CI already proves is
repeated as an operator step. And no individual station's evidence trail is kept here.

## Migrating From A Predecessor Agent

The admin-side procedure for a station already running a *differently-named* localhost print agent,
and the counterpart to [[packaging#Packaging#Station Installer#Migration Is Not Automatic]], which
records why the installer refuses to do it for you.

Because `preinstall` stops only its own launchd label and the vendor's Browser Print, everything
else is four steps an admin takes first: identify the port holder with `lsof`, remove it with **its
own uninstaller**, confirm 9100 and 9101 are free, then install normally. The uninstaller-not-`rm`
rule is the load-bearing one — a launchd job deleted but not booted out comes back, and an orphaned
trusted certificate stays trusted. Copy the predecessor's log before its uninstaller deletes it.

Two after-effects are expected rather than faults, and the runbook says so in the words an admin
will otherwise file a bug in. The station certificate is generated and trusted afresh, because the
cert directory is named after the product and this agent finds an empty one — so Safari's trust for
`https://localhost:9101` is established once more, and the predecessor's own `localhost`
certificate may still sit in the System keychain if its uninstaller left it. And the caller
re-picks its printer exactly once, because [[tools#Print Agent#Discovery And Stable Identity]]
hashes the raw device URI while a predecessor may have keyed on the queue name.

### Telling Two Agents Apart

An installed agent is identified by where it lives — binary path, launchd label, installer receipt,
uninstaller, and the `provider` field every `Device` carries — never by its version string.

Two builds of this product family can both report `0.1.0`, and nothing at the wire can fix that:
the contract is frozen, `X-Print-Agent-Version` is the only version channel the product has, and
the `Device` shape can never grow a field naming the product
([[tools#Print Agent#Version And Health Surface]]). So the runbook's identification table reads
disk and launchd state instead, and `pkgutil --pkgs` is the single command that exposes a
predecessor still registered on the station.

## Rollback Path

Rolling a station back is installing the previous release's `.pkg` over the running one — no
uninstall, no cleanup, no flag, no special mode. It is a documented one-step operation because
three properties of the package make it one.

`preinstall` boots out any prior copy of this agent before it checks the ports, so a running build
never blocks its own replacement. `distribution.xml` gates on host architecture and minimum macOS
version only, so the package carries **no downgrade guard** and `installer` lays an older payload
over a newer one without complaint. And `postinstall` reuses an existing station cert unless it is
within 30 days of expiry, so a rollback re-trusts nothing and Safari never re-prompts. Any release
that ever shipped is a valid target rather than only the immediately preceding one, which is what
[[infrastructure#Infrastructure#Release Chain#Asset Retention]] exists to guarantee.

The two halves of the path have different standing, and the runbook flags the difference rather
than papering over it: retention is enforced by the release workflow on every run, while "reinstall
the prior package and the station works again" is proven once, on hardware, by item 11 of the
checklist below. Nothing auto-updates, so a rolled-back station stays put until an admin installs a
newer package by hand — the last step is to pin it and file the defect against the bad release.
Rolling back to a version that shipped under a *different* product name is not a rollback at all
but a migration in the other direction, and it meets the same port-freedom hard failure.

## Station Validation Checklist

Eleven items run once per station type, on a real Mac with a real label printer, before any shift
depends on the agent. The checklist exists exactly where automated coverage stops.

[[tests#Tests#Agent Core]] states that boundary from the other side: the recording fake proves the
agent's reaction to a state, never that the state occurs. A USB bus re-enumerating to the same
device URI, a powered-but-unresponsive printer stalling `lpstat`, a ~540 KB `^GFA` payload
rendering on media, a Gatekeeper verdict on a notarized package, launchd bringing the agent back
from a `kill -9` — each needs a station, a printer, and a person. The checklist is the list of
those, and its named setup (a Mac that still has Zebra Browser Print installed, a USB printer, a
second printer on the network, the previous release's package already downloaded) is chosen so
removal, failover and downgrade are genuinely exercised instead of assumed.

The runbook also marks which items need no hands on the machine: over SSH, an unprivileged binary
run straight from `/tmp` against the real CUPS queue settles discovery, the stable uid, real jobs
on real media, the version and CORS surface, the origin allowlist, the TLS listener, and the
honest-error path. What is left over needs root, a GUI, a second printer, a hand on the USB cable,
or a power-state change. Item 11 is the one that runs exactly once ever, because what it validates
is the rollback path itself rather than a station. And a macOS upgrade re-opens the `lp -o raw`
proof recorded in [[tools#Print Agent#CUPS Contract#Capture Provenance]] — raw queues are already
gone and printer drivers are deprecated, so a known-good pairing is a fact with an expiry date.

### The Checklist Is Not A Run Log

What ships is the product's validation procedure. The recorded results of running it against one
lab's hardware do not ship with it: that evidence stayed in the originating private monorepo's own
knowledge graph when this repository was extracted.

The separation is a decision, not an oversight of the extraction. A run log is a claim about named
machines on named dates — station serials, site queue names, observed timings — and is precisely
the class of string [[infrastructure#Infrastructure#Naming Gate|the naming gate]] bans from this
tree with no allowlist and no exception mechanism. A checklist is a claim about what has to be true
of *any* station, which is reusable by anyone who installs the agent, and it is the half a public
repository can carry.

The consequence is how an unchecked box reads here: nobody has run that item against this release,
never that somebody ran it and it failed. Where a station result is load-bearing for something this
graph asserts — the launchd on-demand gate's reproduction and its post-reboot remediation, or the
`lpstat` capture the parsers are tested against — the finding is restated in the graph on its own
merits with the lab identifiers stripped, and the raw log is never the citation.
