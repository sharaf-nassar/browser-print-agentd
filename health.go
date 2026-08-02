package main

import (
	"context"
	"sync"
	"time"
)

// healthCacheTTL bounds how stale a health verdict may be. Short enough that a
// printer that comes or goes is noticed within seconds, long enough that a
// burst of endpoint hits does not spawn an lpstat pair per request.
const healthCacheTTL = 3 * time.Second

// driverCacheTTL bounds how stale a driver verdict may be. It is far longer
// than healthCacheTTL because the two answer different kinds of question: a
// queue's health changes minute to minute, while the driver behind it changes
// only when an operator reinstalls the printer. Re-probing per job would put an
// `lpoptions` fork in front of every page for an answer that is effectively
// constant; never re-probing would mean a long-lived agent kept a stale verdict
// until it was restarted.
const driverCacheTTL = 5 * time.Minute

// healthChecker answers "can this queue actually print right now", caching the
// verdict briefly and probing queues concurrently so a station with several
// printers still answers `GET /available` inside the SPA's 1500 ms probe abort.
type healthChecker struct {
	cups *cupsClient
	ttl  time.Duration
	now  func() time.Time

	mu      sync.Mutex
	entries map[string]healthEntry
}

// healthEntry is one cached verdict and when it was taken.
type healthEntry struct {
	at      time.Time
	healthy bool
}

// newHealthChecker builds a checker over cups with the default cache TTL.
func newHealthChecker(cups *cupsClient) *healthChecker {
	return &healthChecker{
		cups:    cups,
		ttl:     healthCacheTTL,
		now:     time.Now,
		entries: map[string]healthEntry{},
	}
}

// healthy reports whether the queue can print: it must be present, enabled,
// online, AND accepting requests.
//
// All four are required because CUPS hides three distinct failures. A disabled
// queue exits 0 from `lpstat -p`, so the exit code cannot be trusted; and a
// `cupsreject`ed queue still reads "enabled" (observed on macOS 26.4: `lpstat
// -p` prints "is idle. ... Rejecting Jobs" with "enabled since" and exits 0),
// so `-p` alone reports it as healthy. `lpstat -a` is a separate mandatory
// check because it is the only probe that catches that rejection. An
// unreachable device can remain enabled and accepting, so long `lpstat -l -p`
// status is also required to rule out its offline status. Any probe error
// (missing binary, hung device blowing the command timeout) reads as unhealthy
// — never as a printable queue.
func (h *healthChecker) healthy(ctx context.Context, queue string) bool {
	now := h.now()
	h.mu.Lock()
	entry, cached := h.entries[queue]
	h.mu.Unlock()
	if cached && now.Sub(entry.at) < h.ttl {
		return entry.healthy
	}

	healthy := false
	online, err := h.cups.queueOnline(ctx, queue)
	if err == nil && online {
		accepting, err := h.cups.queueAccepting(ctx, queue)
		healthy = err == nil && accepting
	}

	h.mu.Lock()
	h.entries[queue] = healthEntry{at: h.now(), healthy: healthy}
	h.mu.Unlock()
	return healthy
}

// driverChecker answers "does this queue's driver flip the page", caching the
// verdict for driverCacheTTL. It mirrors healthChecker deliberately — same
// cache shape, same injectable clock, same "any probe error is the safe
// answer" rule — because it is the same kind of thing: a per-queue property
// read out of CUPS that a print route must not fork a command for on every job.
type driverChecker struct {
	cups *cupsClient
	ttl  time.Duration
	now  func() time.Time

	mu      sync.Mutex
	entries map[string]driverEntry
}

// driverEntry is one cached driver verdict and when it was taken.
type driverEntry struct {
	at        time.Time
	inverting bool
}

// newDriverChecker builds a checker over cups with the default cache TTL.
func newDriverChecker(cups *cupsClient) *driverChecker {
	return &driverChecker{
		cups:    cups,
		ttl:     driverCacheTTL,
		now:     time.Now,
		entries: map[string]driverEntry{},
	}
}

// inverting reports whether the queue's driver rotates every rendered page 180
// degrees, so a document bound for it needs the counter-rotation.
//
// A probe error — no `lpoptions` binary, a hung device blowing the command
// timeout, an unparseable answer — reads as NOT inverting. That direction is
// deliberate and is the opposite of a "fail closed" instinct: the compensation
// is a rotation, so guessing wrong in the true direction turns an upright page
// upside down on every queue on the station, while guessing wrong in the false
// direction leaves the known bug exactly as it already is. Only a positively
// identified inverting driver gets rotated.
func (d *driverChecker) inverting(ctx context.Context, queue string) bool {
	now := d.now()
	d.mu.Lock()
	entry, cached := d.entries[queue]
	d.mu.Unlock()
	if cached && now.Sub(entry.at) < d.ttl {
		return entry.inverting
	}

	model, err := d.cups.driverModel(ctx, queue)
	inverting := err == nil && invertingDriver(model)

	d.mu.Lock()
	d.entries[queue] = driverEntry{at: d.now(), inverting: inverting}
	d.mu.Unlock()
	return inverting
}

// healthyPrinters filters printers down to the ones that can print, preserving
// the USB-first priority order discovery established. Probes run concurrently
// so total latency is one probe deep rather than one per printer.
func (h *healthChecker) healthyPrinters(ctx context.Context, printers []printer) []printer {
	verdicts := make([]bool, len(printers))
	var wait sync.WaitGroup
	for index, candidate := range printers {
		wait.Add(1)
		go func(index int, queue string) {
			defer wait.Done()
			verdicts[index] = h.healthy(ctx, queue)
		}(index, candidate.Queue)
	}
	wait.Wait()

	usable := make([]printer, 0, len(printers))
	for index, candidate := range printers {
		if verdicts[index] {
			usable = append(usable, candidate)
		}
	}
	return usable
}
