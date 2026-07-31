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
will be; they need a station, a printer, and a person.

### Queue Health Reads Status Text

Verifies `[[cups.go#parseQueueEnabled]]` reads the enabled, disabled, absent, and unrecognised
forms out of `lpstat -p` text, and that the exit code alone cannot decide health because a
disabled queue exits 0 while only an absent one exits 1.

### Rejecting Queue Is Unhealthy

Verifies `[[cups.go#parseQueueAccepting]]` reads the `lpstat -a` accepting and not-accepting
forms, and that a `cupsreject`ed queue — which still reads enabled to `lpstat -p` — is excluded
from the healthy set by that separate check.

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

### Origin Posture Gates Write

Verifies the configured posture: with `--origin-allow` set, a `/write` from a disallowed `Origin`
is rejected non-2xx with no `lp` call, and the rejection is logged.

The allowed origin still prints normally, and in the default unconfigured posture that same
disallowed request is logged and allowed.

### CORS Echoes Origin And Answers Preflight

Verifies the `OPTIONS` preflight answers 204 with the request `Origin` echoed back plus the
allowed methods and headers, and that a request with no `Origin` gets the wildcard — without
which the browser blocks `/write` outright.

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
