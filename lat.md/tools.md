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

`[[health.go#healthChecker#healthy]]` calls a queue usable only when it is present, enabled, AND
accepting requests, and `[[health.go#healthChecker#healthyPrinters]]` filters discovery down to
that set while preserving USB-first order.

All three conditions are required because CUPS hides two separate failures. A DISABLED queue
exits 0 from `lpstat -p` and only an ABSENT queue exits 1, so the exit code carries exactly one
bit and health has to come from the text — trusting the exit code is what produced the original
silent-success bug, where a dead USB queue advertised itself, `lp` accepted the job, and the
caller reported "Sent" for a label that never printed. Separately, a `cupsreject`ed queue still
reads `enabled` to `lpstat -p` while rejecting every job handed to it, which is invisible without
the second `lpstat -a` check. Probes run concurrently under a per-command timeout so a wedged USB
device reads as unhealthy instead of blocking the caller's 1500 ms reachability probe.

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

**`/print-pdf` must never be given `-o raw`, and this is the one invariant a future editor is
likely to break.** The two print routes look symmetrical and the temptation to collapse them into
one lp invocation is real, but the option is correct on exactly one of them. ZPL is
printer-native, so `-o raw` is what stops the filter chain from rasterizing it. A PDF is the
opposite: it is not printer-native, CUPS has to render it, and `-o raw` would push the raw PDF
bytes to the device, which prints them as garbage. Neither mistake produces an error — both
silently print the wrong thing, the exact failure class this agent was built to eliminate — so
[[tests#Agent Core#Print PDF Spools A Document Without Raw]] asserts the presence and the absence
in the same test rather than trusting a comment.

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

`rastertolabel` writes a literal `^POI` — ZPL for "invert 180" — into every job its `ZEBRA_ZPL`
arm produces, with the source comment "Rotate 180 degrees so that the top of the label/page is at
the leading edge". It is a straight-line `puts` with no branch, no PPD option, and no device
default behind it, so nothing handed to `lp` can switch it off. The PPD (`Zebra ZPL Label Printer`,
`*cupsFilter: "application/vnd.cups-raster 50 rastertolabel"`) exposes no orientation option at
all. Three prints settled it: ZPL pinning `^PON` printed upright, ZPL with no `^PO` printed
inverted, and a PDF through `/print-pdf` printed inverted.

The only lever left is upstream of the filter, so `/print-pdf` passes `-o orientation-requested=6`
— IPP reverse-portrait — which makes the PDF-to-raster stage hand `rastertolabel` an
already-inverted raster that its own `^POI` rights again. This was measured, not assumed: running
the real chain (`ppdc -d ppd /usr/share/cups/drv/sample.drv`, then `cupsfilter -p ppd/zebra.ppd -m
printer/foo -e`) and decoding the emitted `~DGR:CUPS.GRF` payload back to a bitmap shows the black
bar's pixel mass moving from the top quarter to the bottom quarter (234904/9744 → 9744/234904)
while `^POI` stays in the output unchanged. `=3` is byte-identical to passing nothing; `=4`, `=5`
and `-o landscape` rotate 90° and crop.

**Two things about this must not be "simplified".**

**It is conditional, and unconditional rotation would be a worse bug than the one it fixes.** The
obvious edit — always pass the option, since the station is a label printer anyway — inverts a
correct page on every queue whose driver does not flip, and does it silently, which is the failure
class this agent exists to eliminate. `[[tests#Agent Core#Print PDF Leaves A Non Inverting Driver
Alone]]` fails if the option ever reaches a non-flipping queue.

**It is keyed on the DRIVER, not on "the queue uses `rastertolabel`".** That one filter binary
drives six label languages off the PPD's `*cupsModelNumber`, and the `^POI` lives in exactly one of
them. A DYMO, CPCL, or EPL queue runs the same binary and does NOT flip, so filter-based detection
would invert three families of printer that print correctly today. It is equally not keyed on
"Zebra": a Zebra queue driven by a vendor PPD or by IPP Everywhere never reaches this filter.

The probe is `lpoptions -p <queue>`, read for `printer-make-and-model` — the PPD's `*NickName`
republished over IPP. The PPD file itself is not the source because CUPS writes
`/etc/cups/ppd/<queue>.ppd` as `0640 root:lp` and the agent does not run as root, so a file-based
probe would fail on every station and the compensation would silently never apply.
`[[health.go#driverChecker]]` caches the verdict for five minutes, next to the health cache and for
the opposite reason: health changes minute to minute, a driver changes only when an operator
reinstalls the printer. Any probe failure reads as "does not flip" — the direction that leaves the
known bug in place rather than inverting every queue on the station on a guess.

The verdict is taken for the queue `[[server.go#agent#resolveTarget]]` actually returned, not the
one the caller asked for. Failover can move a job to a different printer with a different driver,
and compensating for a printer the job did not go to is how this fix becomes the bug.

## Origin Posture

The loopback surface has no token, so any page the operator visits could otherwise enumerate
printers and spool ZPL or a rendered PDF. v1 accepts that risk only on the condition that it is
auditable rather than silent.

`[[log.go#agentLogger#request]]` records the `Origin` of every request, and
`[[log.go#agentLogger#job]]` records each job's outcome with the device uid, byte count, `lp`
request id, and origin — the minimal audit trail v1 commits to (a formal retention policy is a
non-goal). `--origin-allow` takes an optional comma-separated allowlist
(`[[server.go#parseOriginAllow]]`); when it is configured,
`[[server.go#agent#originAllowed]]` rejects a print request from any other origin — including one
with no `Origin` header — before any `lp` call runs, and logs the rejection. Unconfigured, the
default, every origin is logged and allowed. This is additive to CORS and never changes a
caller's own successful path.

Both write-capable routes go through the one gate. `[[server.go#agent#enforceOrigin]]` is what
`/write` and `/print-pdf` share, deliberately rather than incidentally: a second route that
spools to a printer but skipped the allowlist would make the allowlist bypassable by posting a
PDF instead of ZPL, so the check is structural and is asserted on both routes.

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
