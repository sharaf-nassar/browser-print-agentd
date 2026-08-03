---
lat:
  require-code-mention: true
---

# Tests

Test specifications for this repository. Every section here is a claim about behaviour that some
test in the tree is responsible for proving, tied to that test by a `// @lat:` comment placed
next to it.

## Agent Core

Pins the daemon itself: the hardware-captured `lpstat` parsing, discovery and stable identity,
health-gated failover, the origin posture, and the version surface. Covered by `agent_test.go`.

The suite fills the agent's `execRunner` seam with a recording fake that answers `lpstat` from a
per-test queue-state map and records `lp` without executing it, so nothing touches CUPS, no label
is ever spooled, and a test can assert that a rejected request made no `lp` call at all. The
`lpstat` strings the fake emits are the ones captured verbatim on the spike hardware described in
[[tools#Print Agent#CUPS Contract#Capture Provenance]], so the parsers are exercised against real
CUPS output rather than a paraphrase. HTTP cases run against a live server on an ephemeral
loopback port.

The fake is also where this suite's coverage stops, and the boundary is deliberate. It can prove
the agent's REACTION to a state — a wedged probe timing out, a hashed uid, a ~540 KB payload
reaching `lp` byte-identical — but never that the state occurs on real hardware. So a USB bus
actually re-enumerating to the same device URI, a powered-but-unresponsive printer actually
stalling `lpstat`, and that payload actually rendering on media are not tested here and never
will be; they need a station, a printer, and a person. Those are enumerated as
[[operations#Station Operations#Station Validation Checklist]], which is the other half of this
boundary rather than a second test suite.

### Queue Health Reads Status Text

Verifies `[[cups.go#parseQueueEnabled]]` reads the enabled, disabled, absent, and unrecognised
forms out of `lpstat -p` text, and that the exit code alone cannot decide health because a
disabled queue exits 0 while only an absent one exits 1.

### Rejecting Queue Is Unhealthy

Verifies `[[cups.go#parseQueueAccepting]]` reads the `lpstat -a` accepting and not-accepting
forms, and that a `cupsreject`ed queue — which still reads enabled to `lpstat -p` — is excluded
from the healthy set by that separate check.

### Offline Queue Is Unhealthy

Verifies a real-shape `lpstat -l -p` response carrying `The printer is offline.` and its
`offline-report` alert overrides the enabled and accepting states.

The offline queue is omitted from `/available` and `/default`, `/write` fails over, and no
healthy destination fails loudly.

### Device URI Discovery And Classification

Verifies `[[cups.go#parseDeviceURIs]]` parses `lpstat -v` rows, that
`[[discovery.go#classifyConnection]]` classifies by URI scheme alone across usb, socket, ipp,
ipps, dnssd, lpd, smb, http and https, and that discovery orders USB ahead of network regardless
of CUPS's own order.

### Stable Uid Hashes The Raw Device URI

Verifies `[[discovery.go#stableUID]]` hashes the raw percent-encoded device URI — so the uid
survives a queue rename, differs between two units that differ only by serial, and never matches
a decoded form — and falls back to the queue name without a serial.

### Available Lists Healthy Printers USB First

Verifies `GET /available` returns the frozen `Device` shape USB-first, omits a disabled printer
and a rejecting one, and degrades to an empty list when nothing can print, so a caller can never
pin a printer that would swallow a label.

### Default Returns Empty Body Without A Printer

Verifies `GET /default` answers with the highest-priority healthy printer, and with a genuinely
EMPTY body rather than an empty object when none is healthy, because the transport reads empty
text as "no default printer".

### Write Fails Over From Dead USB To Network

Verifies a `/write` naming a printer that died between listing and writing spools to the healthy
network printer with a 200 and an explicit fallback log line.

A healthy pinned printer still prints on itself with no fallback line. This is the agent's one
deliberate divergence from the reference implementation, which 500s the same request.

### Write Fails Loudly With No Healthy Printer

Verifies `/write` returns a plain-text non-2xx and makes no `lp` call when no printer on the
station is usable, and rejects a body with no `data` as a 400, so a dead station can never report
a phantom "Sent".

### Write Streams Large Payloads Verbatim

Verifies a ~540 KB `^GFA` payload — the size of a 4×6 label — reaches `lp` byte-identical and
that the invocation carries `-o raw`, without which the `zebra.ppd` filter would rasterize the
ZPL and print the label as a picture of itself.

### Print PDF Spools A Document Without Raw

Verifies `POST /print-pdf` for an ordinary non-inverting queue decodes its base64 payload, spools
it byte-identically, and invokes `lp` as exactly `-d <queue> <file>` with **no** `-o raw`, while the
same run proves `/write` still carries `-o raw`.

The two routes are asserted apart rather than each in isolation because the failure they guard
against is symmetrical and silent. `-o raw` on a PDF hands unrendered bytes to the device;
its absence on ZPL lets `zebra.ppd` rasterize the label. Neither errors — both print the wrong
thing — so a future editor "unifying" the two lp invocations has to break a named assertion.

### Print PDF Converts An Inverting Driver To Inline Graphics

Verifies a PDF bound for the stock inverting ZPL driver is rendered offline and spooled as bounded
upright inline ZPL, while `/write` remains unchanged.

The fake renderer must receive the source PDF exactly once for the resolved queue. The first `lp`
payload then contains `^PON`, explicit geometry, and uncompressed `^GFA` with pixels rotated into
the authored orientation; `~DG`, `^XG`, `^ID`, and `^POI` are forbidden. Only that generated ZPL
is submitted with `-o raw`, and the sibling `/write` path remains printer-native and untouched.

### Raster Label Transformation Is Bounded And Fail Closed

Verifies captured stored graphics become upright inline row bands within ZPL field and agent byte
limits, while malformed or unknown filter output is rejected.

One raster large enough to cross 99,999 bytes must become two whole-row `^GFA` fields with
explicit origins and label dimensions. Unexpected commands, inconsistent declared sizes,
out-of-width pixels, and binary output each fail rather than being forwarded. Multiple pages stay
separate bounded formats under one raw submission.

### Print PDF Rejects Unsafe Filter Output Before Spooling

Verifies unexpected output from the offline stock-driver filter produces a plain print failure
before any `lp` call can reach a printer.

The renderer is still called exactly once, which distinguishes a parser refusal from a route or
driver-detection failure. Neither the page nor a restoration job is submitted because printer
state has not changed.

### Queue PPD Validation Pins The Stock ZPL Filter

Verifies offline conversion accepts only a safe queue name and a PPD declaring the exact stock ZPL
model number, identity, and rastertolabel filter.

Removing any required directive or introducing a path-bearing queue name fails before cupsfilter.
This keeps a queue reconfiguration from feeding an unrelated printer language to the strict ZPL
parser.

### Print PDF Restores Saved Printer Orientation

Verifies generated inline ZPL for an inverting driver is immediately followed by raw `^JUR` on the
same queue, while a PDF for a non-inverting driver emits no restoration job.

The restore recalls the last `^JUS`-saved configuration rather than hardcoding `^PON`, so the
agent returns shared printer state to the operator's chosen baseline. Its raw submission is
load-bearing: sending `^JUR` through the document filter would rasterize the command instead of
executing it.

### Print PDF Leaves A Non Inverting Driver Alone

Verifies no offline renderer, raw ZPL, or orientation option affects a Zebra CPCL queue (the SAME
filter binary, a different label language), a queue with no driver, one publishing no model, or
one whose probe fails.

This is the half that makes the fix safe rather than merely effective. An unconditional rotation
would double-invert every one of these cases, turning a correct page upside down on printers that
were never broken — a strictly worse bug, and an equally silent one. The CPCL case is the sharpest
of the four: it proves detection is keyed on the DRIVER and not on "the queue uses `rastertolabel`",
which would have inverted three label families that share the binary but not the `^POI`. The
failed-probe case pins the direction of the safe default: an unanswerable probe means no rotation.

### Driver Detection Reads The Queue Model And Caches It

Verifies `[[cups.go#parseDriverModel]]` pulls `printer-make-and-model` out of a crowded one-line
`lpoptions -p` answer — quoted, unquoted, absent, key-shadowed — and that
`[[cups.go#invertingDriver]]` matches the ZPL driver but not its CPCL sibling or an IPP Everywhere
Zebra.

It also pins the caching contract in `[[health.go#driverChecker#inverting]]`: three jobs to one
queue fork the probe once, while a second queue is probed separately. A queue's driver does not
change between jobs, so re-forking `lpoptions` per page would put a command in front of every
print for an answer that is effectively constant.

### Print PDF Rejects A Payload That Is Not A PDF

Verifies a `data` field that will not base64-decode, one that decodes to something without the
`%PDF` signature, an empty one, and a missing one are each a plain-text `400`, and that none of
them reaches `lp`.

Validation precedes every CUPS call on purpose: CUPS would accept arbitrary bytes and put garbage
on media rather than fail, so the route has to refuse the payload itself.

### Print PDF Caps The Body At Fifty Megabytes

Verifies a body past the 50 MB `/print-pdf` cap is a plain-text `413` that spools nothing, and
that a document inside the cap still prints. The cap is enforced before decoding, so an abusive
body is never expanded in memory.

### Print PDF Shares Write Failover And Origin Gating

Verifies `/print-pdf` reuses `[[server.go#agent#resolveTarget]]` rather than re-implementing it:
it fails over from a dead pinned printer with an explicit `print-pdf fallback` log line, and on a
station with nothing healthy it returns a plain-text non-2xx with no `lp` call.

It also pins the security property that makes the route safe to add. `/print-pdf` spools to a
physical printer, so it is gated by `--origin-allow` exactly as `/write` is — a disallowed origin
and a request with no `Origin` header are both `403` before any CUPS work, and the rejection is
logged. A route that skipped the allowlist would make the allowlist bypassable by posting a PDF
instead of ZPL. The CORS origin echo and `X-Print-Agent-Version` are asserted on the new error
path and preflight too, since a browser that cannot read a `400` sees an opaque network failure
instead of the reason.

### Origin Posture Gates Write

Verifies the configured posture: with `--origin-allow` set, a `/write` from a disallowed `Origin`
is rejected non-2xx with no `lp` call, and the rejection is logged.

The allowed origin still prints normally, and in the default unconfigured posture that same
disallowed request is logged and allowed.

### CORS Echoes Origin And Answers Preflight

Verifies the `OPTIONS` preflight answers 204 with the request `Origin` echoed back plus the
allowed methods and headers, and that a request with no `Origin` gets the wildcard — without
which the browser blocks `/write` outright.

### Private Network Preflight Grant

Verifies the Private Network Access grant: returned when the preflight asks for it, absent when
it does not, and governed by the origin allowlist once one is configured.

The unasked case is pinned so the header is not sprayed onto ordinary preflights, and the
allowlist case is pinned because an origin refused at `/write` must not collect a private-network
grant here. What this does **not** pin is a station working, and the distinction is worth keeping
straight: the grant was written against a misdiagnosed outage
([[tools#Print Agent]]), and no station is known to need it. Chrome 142
moved to Local Network Access, which gates on a permission rather than a preflight, so a current
Chromium sends no `Access-Control-Request-Private-Network` at all and never reaches the branch
under test. These assertions guard a PNA-era client that still asks, and nothing more.

### Read And Unknown Routes

Verifies `POST /read` answers 200 with an empty body for parity with the vendor daemon, and that
an unknown path is a plain-text 404 rather than an HTML error page.

### Version Surfaces On Health And Every Response

Verifies the `-ldflags` seam a release writes to — `[[version.go#version]]` — reaches both
diagnostics surfaces: the `X-Print-Agent-Version` header on every response including a 404 and a
preflight, and the `GET /health` body.

The test also pins what makes `/health` worth having. It reports EVERY discovered queue with its
verdict, including the disabled one that `/available` hides so a caller cannot pin it, and it
still answers 200 with the version when the station has no usable queue at all — a station whose
CUPS is broken has to be able to tell you what version is running.

### Config Resolves Flags Over Environment

Verifies `[[config.go#parseConfig]]` resolves flags over environment over built-in defaults, that
the defaults are loopback with the two ports the frozen transport probes, and that the cert pair
resolves under the per-user Application Support path.
