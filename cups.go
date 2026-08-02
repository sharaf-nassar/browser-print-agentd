package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// The CUPS strings below keep the exact shape captured on the hardware spike
// (Apple M1 Max, macOS 26.4 build 25E246, CUPS 2.3.4, ZD621 attached over USB)
// and are the parser's real input, not paraphrases. Queue names and the device
// serial are placeholders — no real station's values appear here:
//
//	printer zd621_net is idle.  enabled since Fri Jul 24 16:02:07 2026
//	printer zd621_net disabled since Fri Jul 24 16:02:21 2026 -
//	<TAB>reason unknown
//	printer zd621_usb is idle.  enabled since Fri Jul 24 16:02:07 2026
//	<TAB>The printer is offline.
//	<TAB>Alerts: offline-report connecting-to-device
//	lpstat: Invalid destination name in list "nosuchqueue".
//	zd621_net accepting requests since Fri Jul 24 16:02:07 2026
//	zd621_net not accepting requests since Fri Jul 24 16:02:21 2026 -
//	<TAB>Rejecting Jobs
//	device for zd621_usb: usb://Zebra%20Technologies/ZTC%20ZD621-300dpi%20ZPL?serial=SN0000000001
//	copies=1 … printer-make-and-model='Zebra ZPL Label Printer' printer-state=3 …
//
// Note the two spaces after "idle.", the trailing " -" plus tab-indented
// continuation line on both unhappy forms, and that `lpstat -v` carries NO
// direct/network class prefix (that prefix only exists in `lpinfo -v`, which
// enumerates devices rather than queues). The last line is `lpoptions -p
// <queue>`: ONE line of space-separated key=value pairs, where a value
// containing spaces is single-quoted — which the driver identity always is.

const (
	// enabledMarker / disabledMarker are the only two states `lpstat -p` reports
	// that the agent trusts. Everything else — an unrecognised state, or no line
	// for the queue at all — reads as unusable so an unparseable state can never
	// produce a phantom success.
	enabledMarker  = "enabled since"
	disabledMarker = "disabled since"
	offlineStatus  = "The printer is offline."
	offlineAlert   = "offline-report"

	// deviceURIPrefix introduces every `lpstat -v` line: "device for <queue>: <uri>".
	deviceURIPrefix = "device for "

	// modelOptionKey is the `lpoptions -p <queue>` key carrying the destination's
	// driver identity — CUPS publishes the PPD's *NickName verbatim as the
	// printer-make-and-model attribute.
	modelOptionKey = "printer-make-and-model"

	// invertingDriverModel is the driver identity that rotates every rendered
	// page 180 degrees on its way to the device. See invertingDriver for why the
	// match is on the DRIVER rather than on "uses rastertolabel".
	invertingDriverModel = "zebra zpl label printer"

	// reversePortrait is the IPP orientation-requested enum for "portrait,
	// rotated 180 degrees" (RFC 8011: 3 portrait, 4 landscape, 5
	// reverse-landscape, 6 reverse-portrait). It is the counter-rotation applied
	// to a document bound for an inverting driver.
	reversePortrait = "orientation-requested=6"

	// cupsCommandTimeout bounds every individual lpstat invocation. A wedged USB
	// device can hang `lpstat` indefinitely, and `GET /available` is the SPA's
	// reachability probe under a 1500 ms abort, so a hung device must read as
	// unhealthy rather than block the probe.
	cupsCommandTimeout = 900 * time.Millisecond

	// cupsPrintTimeout bounds the lp invocation. Spooling gets far more headroom
	// than a health probe: a 4x6 label is ~540 KB of uncompressed ^GFA hex, and
	// truncating that job would be worse than answering slowly.
	cupsPrintTimeout = 15 * time.Second

	// Spool-file suffixes. CUPS sniffs content rather than trusting an
	// extension, so these are for a human reading `ls` on a wedged station's
	// temp directory, not for the filter chain.
	zplSuffix = ".zpl"
	pdfSuffix = ".pdf"

	// restoreSavedConfiguration recalls the printer's last ^JUS-saved settings.
	// The inverting CUPS label driver sends ^POI, and ^PO is retained by the
	// printer for later formats. Recalling the saved configuration restores the
	// operator's chosen orientation without assuming that it is ^PON.
	restoreSavedConfiguration = "^XA^JUR^XZ"
)

// execResult is the outcome of one external command: CUPS reports state in the
// TEXT it prints, so callers read Stdout and treat ExitCode as a single bit
// (0 = the queue is configured, non-zero = it is absent).
type execResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

type boundedCapture struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (b *boundedCapture) Write(data []byte) (int, error) {
	written := len(data)
	remaining := b.limit - b.buffer.Len()
	if remaining > len(data) {
		remaining = len(data)
	}
	if remaining > 0 {
		_, _ = b.buffer.Write(data[:remaining])
	}
	if remaining < len(data) {
		b.exceeded = true
	}
	return written, nil
}

func (b *boundedCapture) String() string {
	return b.buffer.String()
}

// execRunner is the seam that keeps the agent testable without CUPS. The real
// implementation shells out; unit tests inject a stub so no lp/lpstat binary is
// ever required (and no label is ever spooled).
type execRunner interface {
	Run(ctx context.Context, name string, args ...string) (execResult, error)
}

// osRunner runs commands for real via os/exec.
type osRunner struct{}

// Run executes name with args and captures both streams. A non-zero exit is a
// normal result, not an error — only a missing binary or an expired context is
// returned as an error, because those are the two cases a caller must not
// mistake for "the queue said no".
func (osRunner) Run(ctx context.Context, name string, args ...string) (execResult, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	stdout := boundedCapture{limit: maxFilterOutputBytes}
	stderr := boundedCapture{limit: maxFilterErrorBytes}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := execResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, fmt.Errorf("%s timed out: %w", name, ctxErr)
	}
	if stdout.exceeded {
		return result, fmt.Errorf("%s stdout exceeded %d bytes", name, maxFilterOutputBytes)
	}
	if stderr.exceeded {
		return result, fmt.Errorf("%s stderr exceeded %d bytes", name, maxFilterErrorBytes)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	if err != nil {
		return result, err
	}
	return result, nil
}

// cupsClient wraps the lp/lpstat command surface the agent depends on. It is
// the single CUPS coupling point in the agent: every lp invocation is issued
// here, so the backend can be swapped when driver-backed queues disappear.
type cupsClient struct {
	runner       execRunner
	renderer     documentRenderer
	timeout      time.Duration
	printTimeout time.Duration

	// printOrder lets ordinary jobs spool concurrently, but gives an inverting
	// document and its restoration job exclusive access to the submission
	// stream. CUPS preserves job order within a destination, so no other job
	// issued by this agent can land between the state-changing PDF and ^JUR.
	printOrder sync.RWMutex
}

// newCUPSClient builds a client over runner with the default bounded timeouts.
func newCUPSClient(runner execRunner) *cupsClient {
	renderer := documentRenderer(cupsFilterRenderer{runner: runner})
	if injected, ok := runner.(documentRenderer); ok {
		renderer = injected
	}
	return &cupsClient{
		runner:       runner,
		renderer:     renderer,
		timeout:      cupsCommandTimeout,
		printTimeout: cupsPrintTimeout,
	}
}

// run applies the per-command timeout on top of the caller's context so one
// hung device cannot consume the whole request budget.
func (c *cupsClient) run(ctx context.Context, name string, args ...string) (execResult, error) {
	return c.runFor(ctx, c.timeout, cupsCommandTimeout, name, args...)
}

// runFor runs one command under timeout, falling back to fallback when the
// configured timeout is unset.
func (c *cupsClient) runFor(
	ctx context.Context, timeout time.Duration, fallback time.Duration,
	name string, args ...string,
) (execResult, error) {
	if timeout <= 0 {
		timeout = fallback
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return c.runner.Run(ctx, name, args...)
}

// queueOnline reports whether the queue exists, is enabled, and has no CUPS
// offline status or alert.
//
// The exit code carries exactly one bit and it is NOT health: a DISABLED queue
// exits 0 and an ABSENT queue exits 1. Trusting the exit code is what produced
// the silent-success bug this agent exists to kill, so the enabled/disabled
// verdict comes from the printed text and the exit code only rules out an
// absent queue.
func (c *cupsClient) queueOnline(ctx context.Context, queue string) (bool, error) {
	result, err := c.run(ctx, "lpstat", "-l", "-p", queue)
	if err != nil {
		return false, err
	}
	if result.ExitCode != 0 {
		return false, nil
	}
	return parseQueueEnabled(result.Stdout, queue) &&
		!parseQueueOffline(result.Stdout, queue), nil
}

// queueAccepting reports whether the queue is accepting new jobs.
//
// This is a SEPARATE required check: after `cupsreject` a queue still reads
// "enabled" to `lpstat -p` while rejecting every job handed to it, so
// enabled-but-rejecting is invisible to the enabled check alone.
func (c *cupsClient) queueAccepting(ctx context.Context, queue string) (bool, error) {
	result, err := c.run(ctx, "lpstat", "-a", queue)
	if err != nil {
		return false, err
	}
	if result.ExitCode != 0 {
		return false, nil
	}
	return parseQueueAccepting(result.Stdout, queue), nil
}

// driverModel reports the destination's driver identity — the PPD *NickName
// CUPS republishes as printer-make-and-model — or "" when the queue is absent
// or publishes none.
//
// `lpoptions -p <queue>` is the probe rather than the PPD file itself because
// it is cheap, works over IPP, and does not assume the PPD is readable. Only
// after this probe identifies the exact stock ZPL driver does document
// rendering open and validate that queue's PPD; an unreadable PPD then fails
// that print before any printer submission instead of changing detection.
func (c *cupsClient) driverModel(ctx context.Context, queue string) (string, error) {
	result, err := c.run(ctx, "lpoptions", "-p", queue)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", nil
	}
	return parseDriverModel(result.Stdout), nil
}

// deviceURIs enumerates the configured queues and their CUPS device URIs.
func (c *cupsClient) deviceURIs(ctx context.Context) ([]queueDevice, error) {
	result, err := c.run(ctx, "lpstat", "-v")
	if err != nil {
		return nil, err
	}
	return parseDeviceURIs(result.Stdout), nil
}

// printRaw spools data to queue verbatim and returns the CUPS request id.
//
// `-o raw` is load-bearing, not an optimization: without it the zebra.ppd
// filter rasterizes the ZPL into a ~DGR:CUPS.GRF bitmap (42 source bytes became
// 5553 on the wire) and the label prints as a picture of itself. It does not
// error — it silently prints the wrong thing — so every ZPL invocation in this
// agent carries it.
func (c *cupsClient) printRaw(ctx context.Context, queue string, data []byte) (string, error) {
	c.printOrder.RLock()
	defer c.printOrder.RUnlock()
	return c.spool(ctx, queue, data, zplSuffix, "-o", "raw")
}

// printDocument spools data as a DOCUMENT and returns the CUPS request id.
// inverting says the destination uses Apple's ZPL label driver. That driver's
// stored-graphic output is rendered offline, rewritten as inline graphics, and
// raw-spooled; every other destination retains the ordinary document path.
//
// A PDF is never handed raw to a printer. Non-inverting queues submit the PDF
// as a document so CUPS runs their normal filter chain. The one known
// inverting driver is filtered OFFLINE, its captured bitmap is rewritten to
// safe upright printer-native ZPL, and only that generated ZPL is raw-spooled.
//
// The conversion is CONDITIONAL and must stay that way. Applying Zebra ZPL to
// another driver's output would replace a correct document with the wrong
// printer language, and it would do so silently.
func (c *cupsClient) printDocument(
	ctx context.Context, queue string, data []byte, inverting bool,
) (string, error) {
	if !inverting {
		c.printOrder.RLock()
		defer c.printOrder.RUnlock()
		return c.spool(ctx, queue, data, pdfSuffix)
	}

	c.printOrder.Lock()
	defer c.printOrder.Unlock()

	renderContext, cancel := context.WithTimeout(ctx, renderTimeout(c.printTimeout))
	filtered, err := c.renderer.Render(renderContext, queue, data)
	cancel()
	if err != nil {
		return "", fmt.Errorf("render inverting PDF: %w", err)
	}
	upright, err := transformRasterLabelZPL(filtered)
	if err != nil {
		return "", fmt.Errorf("transform inverting PDF: %w", err)
	}

	requestID, err := c.spool(ctx, queue, upright, zplSuffix, "-o", "raw")
	if err != nil {
		return "", err
	}

	// Once CUPS has accepted the rendered job, restoring shared printer state is required
	// even if the HTTP client disconnects and cancels ctx. WithoutCancel keeps
	// request values while runFor still gives the lp submission its own bound.
	restoreContext := context.WithoutCancel(ctx)
	if _, err := c.spool(restoreContext, queue, []byte(restoreSavedConfiguration),
		zplSuffix, "-o", "raw"); err != nil {
		return "", fmt.Errorf("restore saved printer configuration: %w", err)
	}
	return requestID, nil
}

// spool writes data to a temp file with suffix and hands it to `lp -d <queue>`
// with options, returning the CUPS request id. Both print paths share it so the
// spool-file lifecycle, the print timeout, and the failure text stay identical
// no matter what is being printed; only the suffix and the options differ.
func (c *cupsClient) spool(
	ctx context.Context, queue string, data []byte, suffix string, options ...string,
) (string, error) {
	file, err := os.CreateTemp("", tempPrefix+"-*"+suffix)
	if err != nil {
		return "", fmt.Errorf("create spool file: %w", err)
	}
	path := file.Name()
	defer os.Remove(path)
	if _, err := file.Write(data); err != nil {
		file.Close()
		return "", fmt.Errorf("write spool file: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close spool file: %w", err)
	}

	args := make([]string, 0, len(options)+3)
	args = append(args, "-d", queue)
	args = append(args, options...)
	args = append(args, path)

	result, err := c.runFor(ctx, c.printTimeout, cupsPrintTimeout, "lp", args...)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" {
			detail = "lp failed"
		}
		return "", fmt.Errorf("%s", detail)
	}
	return parseRequestID(result.Stdout), nil
}

// parseQueueEnabled reads a queue's enabled/disabled state out of `lpstat -p`
// output. Only an explicit "enabled since" counts as usable; a "disabled since"
// line, an unrecognised line, and no line at all all read as unusable.
//
// Field splitting absorbs both spike quirks for free: the two spaces after
// "idle." and the trailing " -" on the disabled form. Tab-indented continuation
// lines ("reason unknown") never match the "printer <queue>" shape.
func parseQueueEnabled(output string, queue string) bool {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "printer" || fields[1] != queue {
			continue
		}
		state := strings.Join(fields[2:], " ")
		if strings.Contains(state, disabledMarker) {
			return false
		}
		return strings.Contains(state, enabledMarker)
	}
	return false
}

// parseQueueOffline reads the long `lpstat -l -p` status attached to queue.
// CUPS leaves an unreachable device enabled, so its explicit offline status or
// offline-report alert must override the enabled header. Status belonging to a
// different queue is ignored.
func parseQueueOffline(output string, queue string) bool {
	selected := false
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "printer" {
			selected = fields[1] == queue
			continue
		}
		if !selected {
			continue
		}
		status := strings.TrimSpace(line)
		if status == offlineStatus {
			return true
		}
		if strings.HasPrefix(status, "Alerts:") &&
			strings.Contains(status, offlineAlert) {
			return true
		}
	}
	return false
}

// parseQueueAccepting reads a queue's accepting/rejecting state out of
// `lpstat -a` output. "not accepting requests" is checked before "accepting
// requests" because the rejecting line contains the accepting line's words, and
// an unrecognised or missing line reads as NOT accepting.
func parseQueueAccepting(output string, queue string) bool {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != queue {
			continue
		}
		state := strings.Join(fields[1:], " ")
		if strings.HasPrefix(state, "not accepting requests") {
			return false
		}
		return strings.HasPrefix(state, "accepting requests")
	}
	return false
}

// queueDevice is one `lpstat -v` row: a queue name and its raw device URI.
type queueDevice struct {
	Queue string
	URI   string
}

// parseDeviceURIs turns `lpstat -v` output into queue/URI pairs, preserving
// CUPS's own order. The URI is kept EXACTLY as reported — no percent-decoding,
// no case folding — because it is hashed into the stable device uid.
func parseDeviceURIs(output string) []queueDevice {
	devices := make([]queueDevice, 0, 4)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, deviceURIPrefix) {
			continue
		}
		rest := line[len(deviceURIPrefix):]
		colon := strings.Index(rest, ": ")
		if colon <= 0 {
			continue
		}
		queue := rest[:colon]
		uri := rest[colon+2:]
		if queue == "" || uri == "" {
			continue
		}
		devices = append(devices, queueDevice{Queue: queue, URI: uri})
	}
	return devices
}

// parseDriverModel pulls printer-make-and-model out of a `lpoptions -p <queue>`
// line. The whole destination is reported as one line of space-separated
// key=value pairs, and a value carrying spaces — which a driver NickName always
// does — is single-quoted, so the value is read to its closing quote rather
// than to the next space. An unquoted or unterminated value falls back to the
// remainder of the field, and a missing key reads as "" so the caller treats an
// unrecognisable answer as "no compensation" rather than guessing.
func parseDriverModel(output string) string {
	const marker = modelOptionKey + "="
	for _, line := range strings.Split(output, "\n") {
		index := strings.Index(line, marker)
		if index < 0 {
			continue
		}
		// Only a whole field counts: "cups-printer-make-and-model=" must not
		// match, or a future CUPS key could shadow the real one.
		if index > 0 && line[index-1] != ' ' && line[index-1] != '\t' {
			continue
		}
		value := line[index+len(marker):]
		if !strings.HasPrefix(value, "'") {
			fields := strings.Fields(value)
			if len(fields) == 0 {
				return ""
			}
			return fields[0]
		}
		value = value[1:]
		if end := strings.Index(value, "'"); end >= 0 {
			value = value[:end]
		}
		return strings.TrimSpace(value)
	}
	return ""
}

// invertingDriver reports whether a driver identity is one that rotates every
// rendered page 180 degrees before it reaches the device.
//
// CUPS's own label driver emits a literal `^POI` — ZPL for "invert 180" — in
// every job it produces, with the source comment "Rotate 180 degrees so that
// the top of the label/page is at the leading edge". It is a straight-line
// write with no branch and no PPD option behind it, so nothing passed to `lp`
// can switch it off; the only lever is to rotate the raster the other way
// before the filter sees it.
//
// The match is on the DRIVER identity, not on "the queue uses rastertolabel",
// because that filter drives six different label languages off the PPD's
// *cupsModelNumber and the `^POI` lives in exactly one of them (`ZEBRA_ZPL`).
// A DYMO, CPCL, or EPL queue runs the same binary and does NOT flip, so keying
// on the filter would invert three families of printer that were printing
// correctly. It is also not keyed on "Zebra": a Zebra queue driven by a
// vendor PPD or by IPP Everywhere does not go through this filter at all.
//
// Matching is substring-on-normalised-text so a PPD that appends a revision or
// a qualifier to the NickName still resolves, while an unrelated driver cannot
// collide with a four-word phrase.
func invertingDriver(model string) bool {
	return strings.Contains(strings.Join(strings.Fields(strings.ToLower(model)), " "),
		invertingDriverModel)
}

// parseRequestID pulls the job id out of lp's "request id is <id> (1 file)"
// acknowledgement, returning "" when lp said something else.
func parseRequestID(stdout string) string {
	line := strings.TrimSpace(stdout)
	if line == "" {
		return ""
	}
	const marker = "request id is "
	index := strings.Index(line, marker)
	if index < 0 {
		return line
	}
	fields := strings.Fields(line[index+len(marker):])
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}
