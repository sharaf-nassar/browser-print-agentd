package main

import (
	"context"
	"net/http"
)

// version is the release the running binary was cut from.
//
// It is injected at link time by `.github/workflows/release.yml` with
// `-ldflags "-X main.version=X.Y.Z"`, taken from the `vX.Y.Z` tag that
// triggered the release. A build made any other way — a developer's
// `go build`, the Mac-less CI cross-build, `go test` — keeps the default, so an
// unversioned binary on a station is identifiable as such instead of claiming a
// release it did not come from.
var version = "dev"

// versionHeader carries the running version on every response.
//
// It lives here and on `GET /health` and NOWHERE else: the frozen Browser Print
// `Device` shape must never grow a version field, because the SPA parses it and
// pins it in localStorage. A response header is additive — the transport reads
// only the status and the body — so this cannot perturb the wire contract.
const versionHeader = "X-Print-Agent-Version"

// Origin postures reported by the diagnostics endpoint, matching the Q14
// decision: unconfigured means every origin is logged and allowed, configured
// means /write is allowlisted.
const (
	posturelogAndAllow = "log-and-allow"
	postureAllowlist   = "allowlist"
)

// healthReport is the `GET /health` body: what version is running, what origin
// posture it is enforcing, and what CUPS looks like from where the agent sits.
type healthReport struct {
	Version       string          `json:"version"`
	OriginPosture string          `json:"originPosture"`
	OriginAllow   []string        `json:"originAllow"`
	Printers      []healthPrinter `json:"printers"`

	// CUPSError is set when discovery itself failed. The report is still a 200
	// in that case on purpose: "the agent is alive at version X and cannot see
	// CUPS" is the single most useful triage answer it can give, and returning a
	// 500 would hide the version behind the failure.
	CUPSError string `json:"cupsError,omitempty"`
}

// healthPrinter is one discovered queue with the health verdict that decides
// whether `/available` will list it.
type healthPrinter struct {
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Queue      string `json:"queue"`
	DeviceURI  string `json:"deviceUri"`
	Connection string `json:"connection"`
	Healthy    bool   `json:"healthy"`
}

// handleHealth answers the additive diagnostics endpoint.
//
// This is the ONLY endpoint that reports unhealthy printers. `/available` and
// `/default` deliberately hide them so the SPA can never pin a printer that
// cannot print; triage needs the opposite view — the queue that exists but is
// disabled or rejecting is exactly what the operator is calling about.
func (a *agent) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), availableBudget)
	defer cancel()

	report := healthReport{
		Version:       version,
		OriginPosture: posturelogAndAllow,
		OriginAllow:   append([]string{}, a.originAllow...),
		Printers:      []healthPrinter{},
	}
	if len(a.originAllow) > 0 {
		report.OriginPosture = postureAllowlist
	}

	printers, err := discoverPrinters(ctx, a.cups)
	if err != nil {
		report.CUPSError = err.Error()
		sendJSON(w, http.StatusOK, report)
		return
	}

	// Reuses the concurrent probe `/available` runs rather than adding a second
	// health path, then inverts it: everything discovered is reported, flagged
	// by whether it survived the filter.
	usable := a.health.healthyPrinters(ctx, printers)
	healthy := make(map[string]bool, len(usable))
	for _, candidate := range usable {
		healthy[candidate.Queue] = true
	}
	for _, candidate := range printers {
		report.Printers = append(report.Printers, healthPrinter{
			Name:       candidate.Name,
			UID:        candidate.UID,
			Queue:      candidate.Queue,
			DeviceURI:  candidate.DeviceURI,
			Connection: candidate.Connection,
			Healthy:    healthy[candidate.Queue],
		})
	}
	sendJSON(w, http.StatusOK, report)
}
