package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// availableBudget bounds the read-only endpoints. The SPA uses
	// `GET /available` as its reachability probe under a 1500 ms abort, so the
	// discovery + health pass must finish well inside that or the station reads
	// as "no agent" instead of "no printer".
	availableBudget = 1200 * time.Millisecond

	// writeBudget bounds a print job end to end. A 4x6 label is ~540 KB of
	// uncompressed ^GFA hex, so spooling gets real headroom that a health probe
	// does not.
	writeBudget = 20 * time.Second

	// maxWriteBody caps a /write body. ZPL never approaches this; a larger body
	// is a mistake or abuse, not a label.
	maxWriteBody = 8 << 20

	// maxPDFBody caps a /print-pdf body. A rendered label sheet is orders of
	// magnitude smaller; a PDF this big is a mistake, not a document. The limit
	// is deliberately far above maxWriteBody because base64 inflates the payload
	// by a third and a multi-cell sheet carries real page content.
	maxPDFBody = 50 << 20

	// pdfMagic is the signature every PDF opens with. Anything else decoded out
	// of 'data' is a bad payload, and printing it would put garbage on media
	// rather than fail.
	pdfMagic = "%PDF"
)

// printRequest is the body both print routes take: the Device the caller echoes
// back plus a payload. `/write` reads Data as raw ZPL and `/print-pdf` reads it
// as a base64-encoded PDF, but the envelope — and therefore printer selection —
// is identical, which is why they share one type.
type printRequest struct {
	Device *struct {
		UID  string `json:"uid"`
		Name string `json:"name"`
	} `json:"device"`
	Data *string `json:"data"`
}

// requestedPrinter returns the device the body names, or "" when unspecified.
// uid is the pinned identity; name is accepted as a fallback for hand-rolled
// requests.
func (r printRequest) requestedPrinter() string {
	if r.Device == nil {
		return ""
	}
	if r.Device.UID != "" {
		return r.Device.UID
	}
	return r.Device.Name
}

// agent serves the Browser Print wire contract on top of CUPS.
type agent struct {
	cups   *cupsClient
	health *healthChecker
	logger *agentLogger

	// originAllow is the Q14 posture. Empty means log-and-allow: every origin is
	// recorded and permitted. Non-empty turns /write into an allowlisted
	// endpoint — a disallowed Origin is rejected before any lp call runs.
	originAllow []string
}

// newAgent wires an agent over an exec runner (the real one in main, a stub in
// tests) and an origin allowlist.
func newAgent(runner execRunner, logger *agentLogger, originAllow []string) *agent {
	cups := newCUPSClient(runner)
	return &agent{
		cups:        cups,
		health:      newHealthChecker(cups),
		logger:      logger,
		originAllow: originAllow,
	}
}

// ServeHTTP routes the four wire-contract endpoints, the two additive endpoints
// (diagnostics and document printing), and the CORS preflight.
//
// Every request is logged with its Origin (Q14): the loopback surface has no
// token, so any page the operator visits can talk to it, and v1 accepts that
// risk only on the condition that it is auditable rather than silent.
func (a *agent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	a.logger.request(r.Method, r.URL.Path, origin)
	setCORSHeaders(w, origin)

	// Set before routing so EVERY response carries it — a 404 and a preflight
	// included. A station whose agent is answering the wrong thing is diagnosed
	// from the response it actually produced, which may well be the 404.
	w.Header().Set(versionHeader, version)

	if r.Method == http.MethodOptions {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/available":
		a.handleAvailable(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/default":
		a.handleDefault(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/write":
		a.handleWrite(w, r, origin)
	case r.Method == http.MethodPost && r.URL.Path == "/print-pdf":
		a.handlePrintPDF(w, r, origin)
	case r.Method == http.MethodPost && r.URL.Path == "/read":
		a.handleRead(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/health":
		a.handleHealth(w, r)
	default:
		sendText(w, http.StatusNotFound, "not found\n")
	}
}

// setCORSHeaders echoes the request Origin (falling back to "*") so the browser
// lets the SPA read the response. Without this /write is browser-blocked.
func setCORSHeaders(w http.ResponseWriter, origin string) {
	allow := origin
	if allow == "" {
		allow = "*"
	}
	header := w.Header()
	header.Set("Access-Control-Allow-Origin", allow)
	header.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	header.Set("Access-Control-Allow-Headers", "Content-Type")
}

// handleAvailable lists ONLY printers that can print, USB first. An unhealthy
// printer is simply absent, so the SPA can never pin one that cannot print, and
// a station with nothing usable sees the real agent's empty list.
func (a *agent) handleAvailable(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), availableBudget)
	defer cancel()

	usable, err := a.usablePrinters(ctx)
	if err != nil {
		sendText(w, http.StatusInternalServerError, err.Error()+"\n")
		return
	}
	devices := make([]wireDevice, 0, len(usable))
	for _, candidate := range usable {
		devices = append(devices, candidate.device())
	}
	sendJSON(w, http.StatusOK, map[string]any{"printer": devices})
}

// handleDefault returns the highest-priority healthy printer as a single Device
// object, or an EMPTY body when none is healthy — the transport reads empty
// text as "no default", so an empty JSON object here would be a contract break.
func (a *agent) handleDefault(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), availableBudget)
	defer cancel()

	usable, err := a.usablePrinters(ctx)
	if err != nil {
		sendText(w, http.StatusInternalServerError, err.Error()+"\n")
		return
	}
	if len(usable) == 0 {
		sendText(w, http.StatusOK, "")
		return
	}
	sendJSON(w, http.StatusOK, usable[0].device())
}

// handleRead answers the real agent's status endpoint with an empty body. Dead
// surface for the SPA, kept so the agent stays a drop-in for any other caller.
func (a *agent) handleRead(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, io.LimitReader(r.Body, maxWriteBody))
	sendText(w, http.StatusOK, "")
}

// handleWrite spools raw ZPL, failing over rather than failing when the pinned
// printer died between listing and writing.
func (a *agent) handleWrite(w http.ResponseWriter, r *http.Request, origin string) {
	// Origin enforcement runs FIRST: when an allowlist is configured a
	// disallowed origin must be rejected before any CUPS work, so a hostile page
	// cannot spool a label or even enumerate health through timing.
	if !a.enforceOrigin(w, "write", origin) {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxWriteBody+1))
	if err != nil {
		sendText(w, http.StatusBadRequest, "could not read request body\n")
		return
	}
	if len(body) > maxWriteBody {
		sendText(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"request body too large; the /write limit is %d bytes\n", maxWriteBody))
		return
	}

	var payload printRequest
	if err := json.Unmarshal(body, &payload); err != nil {
		sendText(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v\n", err))
		return
	}
	if payload.Data == nil {
		sendText(w, http.StatusBadRequest,
			"missing 'data' (raw ZPL string) in request body\n")
		return
	}
	data := []byte(*payload.Data)

	ctx, cancel := context.WithTimeout(r.Context(), writeBudget)
	defer cancel()

	target, err := a.resolveTarget(ctx, "write", payload.requestedPrinter())
	if err != nil {
		sendText(w, http.StatusInternalServerError, err.Error()+"\n")
		return
	}

	requestID, err := a.cups.printRaw(ctx, target.Queue, data)
	if err != nil {
		a.logger.job("write", false, len(data), target, "", origin)
		sendText(w, http.StatusInternalServerError, err.Error()+"\n")
		return
	}
	a.logger.job("write", true, len(data), target, requestID, origin)
	sendText(w, http.StatusOK, "")
}

// handlePrintPDF spools a base64-encoded PDF — the multi-cell label SHEET a
// caller renders when one ZPL label per print will not do.
//
// It is additive: it sits outside the frozen Zebra-compatible contract and no
// caller of the frozen four is affected by its existence. Everything downstream
// of payload validation is deliberately the SAME code /write runs — the origin
// gate, printer resolution with USB-to-network failover, the job log, and the
// empty-200/plain-text-error convention — so a sheet and a label can never
// disagree about which printer is usable or whether a station is dead.
//
// The one thing that must differ is the lp invocation: see printDocument.
func (a *agent) handlePrintPDF(w http.ResponseWriter, r *http.Request, origin string) {
	// Identical gate to /write, and load-bearing for the same reason. This route
	// spools to a physical printer, so leaving it off the allowlist would make
	// the allowlist trivially bypassable by posting a PDF instead of ZPL.
	if !a.enforceOrigin(w, "print-pdf", origin) {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxPDFBody+1))
	if err != nil {
		sendText(w, http.StatusBadRequest, "could not read request body\n")
		return
	}
	if len(body) > maxPDFBody {
		sendText(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"request body too large; the /print-pdf limit is %d bytes\n", maxPDFBody))
		return
	}

	var payload printRequest
	if err := json.Unmarshal(body, &payload); err != nil {
		sendText(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v\n", err))
		return
	}
	if payload.Data == nil {
		sendText(w, http.StatusBadRequest,
			"missing 'data' (base64-encoded PDF string) in request body\n")
		return
	}

	document, err := base64.StdEncoding.DecodeString(*payload.Data)
	if err != nil {
		sendText(w, http.StatusBadRequest, fmt.Sprintf("invalid base64 in 'data': %v\n", err))
		return
	}
	if !bytes.HasPrefix(document, []byte(pdfMagic)) {
		// Reject rather than spool: CUPS would accept the bytes and the operator
		// would get a page of garbage, which is the silent-wrong-output failure
		// mode this agent exists to prevent.
		sendText(w, http.StatusBadRequest,
			"decoded 'data' is not a PDF (missing "+pdfMagic+" header)\n")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), writeBudget)
	defer cancel()

	target, err := a.resolveTarget(ctx, "print-pdf", payload.requestedPrinter())
	if err != nil {
		sendText(w, http.StatusInternalServerError, err.Error()+"\n")
		return
	}

	requestID, err := a.cups.printDocument(ctx, target.Queue, document)
	if err != nil {
		a.logger.job("print-pdf", false, len(document), target, "", origin)
		sendText(w, http.StatusInternalServerError, err.Error()+"\n")
		return
	}
	a.logger.job("print-pdf", true, len(document), target, requestID, origin)
	sendText(w, http.StatusOK, "")
}

// enforceOrigin applies the Q14 posture to a print route, writing the rejection
// itself and reporting whether the caller may proceed. Both /write and
// /print-pdf go through it so a route that spools to a printer cannot acquire a
// second, weaker copy of the allowlist check.
func (a *agent) enforceOrigin(w http.ResponseWriter, action string, origin string) bool {
	if a.originAllowed(origin) {
		return true
	}
	a.logger.originRejected(action, origin)
	sendText(w, http.StatusForbidden, fmt.Sprintf(
		"origin %q is not allowed to print on this station\n", origin))
	return false
}

// originAllowed applies the Q14 posture: unconfigured means every origin is
// allowed (and logged); configured means only listed origins may print.
// A request with no Origin header is not on any allowlist, so it is rejected
// once one is configured.
func (a *agent) originAllowed(origin string) bool {
	if len(a.originAllow) == 0 {
		return true
	}
	for _, allowed := range a.originAllow {
		if origin == allowed {
			return true
		}
	}
	return false
}

// usablePrinters discovers the station's queues and filters them to the ones
// that can print, keeping USB-first order.
func (a *agent) usablePrinters(ctx context.Context) ([]printer, error) {
	printers, err := discoverPrinters(ctx, a.cups)
	if err != nil {
		return nil, fmt.Errorf("could not enumerate CUPS printers: %w", err)
	}
	return a.health.healthyPrinters(ctx, printers), nil
}

// resolveTarget picks the printer a job goes to, performing device-level
// failover at job initiation.
//
// This is the one deliberate behavioral difference from the dev shim, and it is
// invisible to the transport: the shim 500s a named-but-dead queue, while the
// agent falls over to the next healthy printer (USB before network, since
// discovery already orders them that way), prints, and logs an explicit
// fallback line naming the skipped device. Only when NO printer is healthy does
// the job fail — loudly, never as a phantom "Sent".
func (a *agent) resolveTarget(
	ctx context.Context, action string, requested string,
) (printer, error) {
	printers, err := discoverPrinters(ctx, a.cups)
	if err != nil {
		return printer{}, fmt.Errorf("could not enumerate CUPS printers: %w", err)
	}
	usable := a.health.healthyPrinters(ctx, printers)

	if requested != "" {
		if target, found := matchPrinter(usable, requested); found {
			return target, nil
		}
		if len(usable) == 0 {
			return printer{}, fmt.Errorf(
				"printer %q is not usable and no other printer on this station is "+
					"healthy", requested)
		}
		a.logger.fallback(action, requested, usable[0])
		return usable[0], nil
	}

	if len(usable) == 0 {
		return printer{}, fmt.Errorf(
			"no printer on this station is usable (none enabled and accepting in CUPS)")
	}
	return usable[0], nil
}

// sendJSON writes a JSON body with the CORS headers already set by ServeHTTP.
func sendJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		sendText(w, http.StatusInternalServerError, "could not encode response\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(status)
	w.Write(body)
}

// sendText writes a plain-text body. Every non-2xx the transport sees is plain
// text: the SPA surfaces it verbatim through PrintSendError.
func sendText(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", fmt.Sprint(len(text)))
	w.WriteHeader(status)
	io.WriteString(w, text)
}

// parseOriginAllow splits a comma-separated --origin-allow value into an
// ordered, de-duplicated allowlist. An empty value keeps the default
// log-and-allow posture.
func parseOriginAllow(raw string) []string {
	allow := make([]string, 0, 2)
	for _, chunk := range strings.Split(raw, ",") {
		origin := strings.TrimSpace(chunk)
		if origin == "" {
			continue
		}
		duplicate := false
		for _, existing := range allow {
			if existing == origin {
				duplicate = true
				break
			}
		}
		if !duplicate {
			allow = append(allow, origin)
		}
	}
	return allow
}
