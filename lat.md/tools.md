# Print Agent

The daemon itself: how it reaches CUPS, how it names a printer so a caller's pin survives, how it
decides a queue can actually print, what it logs, and how it reports its own version.

`[[main.go#run]]` starts the listeners the frozen transport probes — plain HTTP on `:9100`
always, HTTPS on `:9101` only when the per-station cert pair exists in `--cert-dir`, both bound
to loopback — and refuses to start at all when `lp`/`lpstat` are absent
(`[[main.go#verifyCUPSBinaries]]`), because an agent that cannot reach CUPS could only ever
report phantom success.

`[[server.go#agent#ServeHTTP]]` routes `GET /available`, `GET /default`, `POST /write`,
`POST /read` and the `OPTIONS` preflight in exactly the shapes the calling transport parses,
echoing the request `Origin` back as `Access-Control-Allow-Origin`. Two additive routes sit beside
them: `GET /health` and `POST /print-pdf` ([[tools#Print Agent#Document Printing]]).
`[[discovery.go#printer#device]]` shapes each wire `Device`; only `name` and `uid` are
load-bearing, and `provider` reports `browser-print-agentd`. Ports and bind address come from
`[[config.go#parseConfig]]` — flag over environment over default — and there is no hand-listed
queue flag, because the agent discovers printers instead of being told about them. Coverage lives
in [[tests#Agent Core]].

Three things differ by design from the Python reference implementation this contract was read
off: printers are discovered from CUPS rather than hand-listed, `/write` fails over instead of
failing when the pinned printer died between listing and writing, and every request's `Origin` is
logged. Everything else is a faithful port, and the frozen wire contract in `README.md` is what
holds it there.

## CUPS Contract

Every CUPS call the agent makes goes through `[[cups.go#cupsClient]]`, which is deliberately the
only coupling point: raw queues are gone from macOS and printer drivers are deprecated, so the
backend has to stay swappable.

`[[cups.go#cupsClient#printRaw]]` always spools ZPL with `lp -d <queue> -o raw <file>`. `-o raw`
is load-bearing rather than an optimization: the hardware spike proved that without it the
`zebra.ppd` filter rasterizes the ZPL into a `~DGR:CUPS.GRF` bitmap (42 source bytes became 5553
on the wire) and the label prints as a picture of itself — it does not error, it silently prints
the wrong thing. `[[cups.go#cupsClient#printDocument]]` omits `-o raw` for exactly the same
reason read the other way, and both go through `[[cups.go#cupsClient#spool]]` so only the lp
options and the temp-file suffix ever differ. Queues themselves are created (by the installer,
not the agent) with
`-m drv:///sample.drv/zebra.ppd`, because `lpadmin -m raw` now exits 1 with
`Raw queues are no longer supported on macOS.` and the `raw  Raw Queue` line `lpinfo -m` still
prints is vestigial and must never be probed as a capability.

Status is read out of the printed TEXT, never the exit code. `[[cups.go#parseQueueEnabled]]`
matches `printer NAME is idle.  enabled since <date>` (two spaces after `idle.`) against
`printer NAME disabled since <date> -` plus its tab-indented `reason unknown` continuation;
`[[cups.go#parseQueueOffline]]` reads the `lpstat -l -p` status `The printer is offline.` and the
`offline-report` alert that CUPS emits while leaving an unreachable device enabled;
`[[cups.go#parseQueueAccepting]]` reads `NAME accepting requests since <date>` against
`NAME not accepting requests since <date> -` plus its `Rejecting Jobs` continuation, checking the
negative form first because the rejecting line contains the accepting line's words. Anything
unrecognised, and any queue with no line at all, reads as unusable so an unparseable state can
never produce a phantom success.

### Capture Provenance

Every `lpstat` string the parsers are tested against was captured on real hardware rather than
written from the man pages. This section records that capture's provenance, and is what the
fixtures in `agent_test.go` cite.

The capture was taken on an Apple M1 Max running macOS 26.4 (build 25E246) with CUPS 2.3.4 and a
Zebra ZD621 attached over USB. What it pinned down is the byte-level shape a parser has to
tolerate: two spaces after `idle.`, the trailing ` -` and tab-indented continuation line on both
the disabled and the rejecting forms, `lpstat -v` rows of the form `device for <queue>: <uri>`
with **no** `direct`/`network` class prefix, a percent-encoded USB device URI carrying a
`serial=` query parameter, and the `lpstat: Invalid destination name in list "<queue>".` text an
absent queue produces.

It also pinned the exit-code trap that the whole health design turns on: a **disabled** queue
exits 0 from `lpstat -p`, only an **absent** queue exits 1, and `lp` accepts a job for a disabled
queue and exits 0. That is why health is read from text, and why `lpstat -a` is a separate probe
rather than an inference.

Two fields are genericized wherever the capture is reproduced in the tree — the queue names and
the device serial. They identify one lab's hardware, no parser reads them, and this repository is
public. Everything else is reproduced byte for byte, quirks included, because paraphrasing the
strings would test the parsers against fiction.

## Discovery And Stable Identity

`[[discovery.go#discoverPrinters]]` enumerates the station's queues from `lpstat -v` and orders
them USB first, then network, each tier keeping CUPS's own order — the priority a hand-written
queue list would otherwise have to supply.

Each `lpstat -v` row is `device for <queue>: <uri>` with NO `direct`/`network` class prefix; that
prefix only exists in `lpinfo -v`, which enumerates devices rather than queues, so
`[[discovery.go#classifyConnection]]` keys on the URI scheme alone: `usb://` is USB and every
other scheme (`socket`, `ipp`, `ipps`, `dnssd`, `lpd`, `smb`, `http`, `https`) is network. Queue
status text is byte-identical for both transports, so the URI is the only place the transport
appears at all.

`[[discovery.go#stableUID]]` derives the identity the caller pins in `localStorage`. A device URI
carrying `serial=` identifies the physical unit across a queue rename and distinguishes two
identical printers on one station, so it is hashed RAW: byte for byte as reported, with no
percent-decoding, no case folding, and no query reordering, since a decode step whose behavior
varies across CUPS or language versions would silently shift every uid on the station and orphan
the pin. A queue whose URI carries no serial falls back to the queue name. Consequence for a
caller migrating from a queue-name-based agent: a pre-existing pin will not match a URI-derived
uid, so the operator re-picks once.

## Health And Failover

`[[health.go#healthChecker#healthy]]` calls a queue usable only when it is present, enabled,
online, AND accepting requests, and `[[health.go#healthChecker#healthyPrinters]]` filters
discovery down to that set while preserving USB-first order.

All four conditions are required because CUPS hides three separate failures. A DISABLED queue
exits 0 from `lpstat -p` and only an ABSENT queue exits 1, so the exit code carries exactly one
bit and health has to come from the text — trusting the exit code is what produced the original
silent-success bug, where a dead USB queue advertised itself, `lp` accepted the job, and the
caller reported "Sent" for a label that never printed. Separately, a `cupsreject`ed queue still
reads `enabled` to `lpstat -p` while rejecting every job handed to it, which is invisible without
the second `lpstat -a` check. An unreachable device can remain both enabled and accepting, so the
`lpstat -l -p` status and alerts must also be checked. Probes run concurrently under a
per-command timeout so a wedged USB device reads as unhealthy instead of blocking the caller's
1500 ms reachability probe.

`[[server.go#agent#resolveTarget]]` is the deliberate divergence: the reference implementation
500s a `/write` naming a dead queue, while this agent falls over to the next healthy printer (USB
before network), prints, returns 200, and logs an explicit fallback line naming the skipped
device. Only when NO printer is healthy does the job fail, with a plain-text non-2xx the caller
surfaces as a send error. The divergence is invisible to the transport, which still just sends
`{device, data}` and reads a status.

## Document Printing

`[[server.go#agent#handlePrintPDF]]` answers `POST /print-pdf`, taking the same `{device, data}`
envelope as `/write` with `data` carrying a base64-encoded PDF. It exists for the multi-cell label
SHEET a caller renders when one ZPL label per print will not do.

**A PDF must be rendered before anything raw reaches the printer.** Most queues receive the PDF
as a normal document with no `-o raw`; [[tests#Agent Core#Print PDF Spools A Document Without Raw]]
pins that default. The stock inverting ZPL driver is the narrow exception: its PPD runs offline,
then only the newly authored printer-native inline ZPL is submitted raw. `/write` remains raw
because its caller already supplied printer-native ZPL. Handing the original PDF raw to either
branch would print document bytes as garbage.

Everything else is deliberately shared rather than duplicated.
`[[server.go#agent#resolveTarget]]` picks the printer and performs the same USB-to-network
failover, `[[server.go#agent#enforceOrigin]]` applies the same allowlist, and success is the same
empty `200` with the same plain-text bodies on failure. Only payload validation is new: a `data`
field that will not base64-decode, or that decodes to bytes not beginning with `%PDF`, is a `400`
before any CUPS call, and a body past 50 MB is a `413`. Both refusals are the route declining to
hand CUPS something it would accept and turn into a wasted page.

The route is ADDITIVE and sits outside the frozen contract in `README.md`: no caller of the five
frozen routes is affected by its existence, and the `Device` shape does not change. A caller that
needs to know whether a station has it reads `X-Print-Agent-Version`, which is still the only
version channel — the route is not advertised through `/available`, so an older agent answers a
sheet request with the plain-text `404` its default arm has always produced.

### Driver-Forced Orientation

A PDF that renders correctly still comes out upside down on a queue driven by CUPS's own label
filter, and the agent — not the caller — is what corrects it. `[[cups.go#invertingDriver]]` decides
whether the destination flips and `[[cups.go#cupsClient#printDocument]]` cancels it.

`rastertolabel` hardcodes two unsuitable choices in its `ZEBRA_ZPL` arm: `^POI` rotates the page
and persists as shared printer state, while `~DGR:CUPS.GRF` plus `^XG` stores and recalls the full
page through printer RAM. The PPD (`Zebra ZPL Label Printer`, `*cupsModelNumber: 18`, and
`*cupsFilter: "application/vnd.cups-raster 50 rastertolabel"`) exposes no option for orientation,
graphic encoding, or storage mode.

Hardware separated those failures. The filter's exact compressed stored graphic printed blank;
expanding only its RLE to full uppercase hex was still blank. The same pixels cropped to a bounded
uncompressed inline `^GFA` printed visible and upright. The driver remains the raster source, but
its device-stored command stream must never be forwarded.

For this driver only, [[zpl_document.go#cupsFilterRenderer#Render]] copies the runtime queue PPD
after validating those three identity directives, then runs absolute `/usr/sbin/cupsfilter` with
`-p <copy> -m printer/foo -e -o orientation-requested=6`. This is filter-only execution: it never
addresses a destination or creates a CUPS job. Reverse portrait turns the raster upstream, exactly
where the old `^POI` expected it.

[[zpl_document.go#transformRasterLabelZPL]] accepts only the captured straight-line
`rastertolabel` grammar. It bounds pages, dimensions, decoded bytes, command output, and every PPD
setting it preserves; any unknown command fails before `lp`. It decodes the stored bitmap, rotates
the pixels 180 degrees to replace `^POI`, and authors `^PON`, explicit `^PW`, `^LL`, `^LH`, and
`^MN` plus uncompressed inline `^GFA`. A field carries at most 99,999 bytes, so a large page is
split into whole-row bands with explicit `^FO0,y` origins. Neither `~DG`, `^XG`, `^ID`, nor `^POI`
can reach the printer through this path.

The generated `^PON` also changes shared printer state. After CUPS accepts the raw generated job,
`printDocument` therefore submits `^JUR` to the same queue. `^JUR` recalls the complete last
`^JUS`-saved configuration, preserving the operator's chosen orientation and media settings
instead of assuming that normal must remain `^PON`.

Submission ordering is part of the correction. `[[cups.go#cupsClient#printRaw]]` and ordinary
document jobs take shared access to the agent's print-order gate, while the inverting PDF holds
exclusive access through offline rendering, generated-ZPL submission, and restoration. CUPS then
sees the generated page and restore as adjacent jobs on the destination; another concurrent agent
request cannot be accepted between them and inherit `^PON`. Once the page is accepted, restoration
uses its own bounded context even if the HTTP request is canceled, because abandoned shared state
is more harmful than an abandoned response.

**Two things about this must not be "simplified".**

**It is conditional, and unconditional conversion would be a worse bug than the one it fixes.**
Running a non-ZPL driver's PDF through this Zebra-specific grammar either fails or authors the
wrong printer language. `[[tests#Agent Core#Print PDF Leaves A Non Inverting Driver Alone]]` fails
if offline rendering or raw ZPL reaches any non-inverting queue.

**It is keyed on the DRIVER, not on "the queue uses `rastertolabel`".** That one filter binary
drives six label languages off the PPD's `*cupsModelNumber`, and the `^POI` lives in exactly one of
them. A DYMO, CPCL, or EPL queue runs the same binary and does NOT flip, so filter-based detection
would invert three families of printer that print correctly today. It is equally not keyed on
"Zebra": a Zebra queue driven by a vendor PPD or by IPP Everywhere never reaches this filter.

The initial probe is `lpoptions -p <queue>`, read for `printer-make-and-model` — the PPD's
`*NickName` republished over IPP. It is cheap and does not require opening every queue's PPD.
`[[health.go#driverChecker]]` caches the verdict for five minutes, next to the health cache and for
the opposite reason: health changes minute to minute, a driver changes only when an operator
reinstalls the printer. Any probe failure reads as "does not flip" — the direction that leaves the
known bug in place rather than rewriting every queue on the station on a guess. Once the exact
driver is known, its PPD becomes render input and must be readable and pass identity validation;
failure there is a plain render error before any printer submission.

The verdict is taken for the queue `[[server.go#agent#resolveTarget]]` actually returned, not the
one the caller asked for. Failover can move a job to a different printer with a different driver,
and compensating for a printer the job did not go to is how this fix becomes the bug.

## Origin Posture

The loopback surface has no token, so any page the operator visits could otherwise enumerate
printers and spool ZPL or a rendered PDF. v1 accepts that risk only on the condition that it is
auditable rather than silent.

`[[log.go#agentLogger#request]]` records the `Origin` of every request, and
`[[log.go#agentLogger#job]]` records each job's outcome with the device uid, byte count, `lp`
request id, and origin — the minimal audit trail v1 commits to. `--origin-allow` takes an optional
comma-separated allowlist
(`[[server.go#parseOriginAllow]]`); when it is configured,
`[[server.go#agent#originAllowed]]` rejects a print request from any other origin — including one
with no `Origin` header — before any `lp` call runs, and logs the rejection. Unconfigured, the
default, every origin is logged and allowed. This is additive to CORS and never changes a
caller's own successful path.

Both write-capable routes go through the one gate. `[[server.go#agent#enforceOrigin]]` is what
`/write` and `/print-pdf` share, deliberately rather than incidentally: a second route that
spools to a printer but skipped the allowlist would make the allowlist bypassable by posting a
PDF instead of ZPL, so the check is structural and is asserted on both routes.

### Request And Job Log Retention

The adopted retention window is a size-bound ring: one active 8 MiB log and seven 8 MiB archives,
for at most 64 MiB of complete audit records per account.

The daemon rotates immediately before appending a complete line that would take `agent.log` over
8 MiB. `agent.log.1` is the newest archive and `agent.log.7` the oldest; the oldest is deleted
when the next generation is created. Archives remain uncompressed so an admin can use ordinary
`grep` and `tail`, and so the stored-byte bound does not depend on compression ratios or temporary
compression files. This is deliberately a size, not age, window: busy stations evict history
sooner, while quiet stations keep it until size pressure or uninstall.

Rotation never splits or rewrites a record. Every retained request line therefore keeps its full
origin, and every retained job line keeps its origin, device uid, byte count, and `lp` request id.
Payload bytes, credentials, printer serials, and site-specific metadata are not added. The log
directory is private mode 0700 and the active file and archives are mode 0600, because origins and
device identifiers are useful audit data but should not be readable by unrelated local accounts.

The mechanism is daemon-owned rotation, specified in
[[packaging#Packaging#Station Installer#Request Log Ownership And Rotation]].
`[[log.go#rotatingLog]]` owns the active descriptor, normalizes the ring on startup, and performs
the close/rename/open sequence under `[[log.go#agentLogger#write]]`'s concurrency boundary.

## Version And Health Surface

`[[version.go#agent#handleHealth]]` answers `GET /health` with the running version, origin posture,
queue health, and safely provable local updater status.

It is the inverse of `/available`, which hides unhealthy printers so a caller can never pin one,
where triage needs to see exactly the queue that exists but cannot print. It still answers 200
when CUPS itself is unreachable, reporting the error alongside the version, because "alive at
version X and blind to CUPS" is the most useful thing it can say.

The same version rides every response as `X-Print-Agent-Version`. `[[version.go#version]]` is the
`-ldflags` seam [[infrastructure#Infrastructure#Release Chain]] links the release tag into; any
other build reports `dev`, so an unversioned binary on a station is identifiable as one. The
version never enters the frozen `Device` shape, which callers parse and pin, so these two
surfaces are the only version channel the product has.

`[[update.go#updateReader#read]]` combines two authorities. The root updater's mode-644 public
file supplies a bounded last-check timestamp, outcome, latest strictly validated manifest
version, and boolean quarantine verdict; its mode-700 private sibling remains unreadable. A
bounded `launchctl print-disabled system` supplies pin truth live. Pinning cannot safely be
file-only: after `launchctl disable` the updater cannot run to rewrite its own file. Missing,
unsafe, malformed, oversized, or ambiguous input therefore omits the `update` object through
`omitempty` instead of failing `/health` or fabricating state. The agent performs no egress and
exposes no update route.
