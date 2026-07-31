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

// healthy reports whether the queue can print: it must be present, enabled, AND
// accepting requests.
//
// All three are required because CUPS hides two distinct failures. A disabled
// queue exits 0 from `lpstat -p`, so the exit code cannot be trusted; and a
// `cupsreject`ed queue still reads "enabled" (observed on macOS 26.4: `lpstat
// -p` prints "is idle. ... Rejecting Jobs" with "enabled since" and exits 0),
// so `-p` alone reports it as healthy. `lpstat -a` is a separate mandatory
// check because it is the only probe that catches that rejection. Any probe
// error (missing binary, hung device blowing the command timeout) reads as
// unhealthy — never as a printable queue.
func (h *healthChecker) healthy(ctx context.Context, queue string) bool {
	now := h.now()
	h.mu.Lock()
	entry, cached := h.entries[queue]
	h.mu.Unlock()
	if cached && now.Sub(entry.at) < h.ttl {
		return entry.healthy
	}

	healthy := false
	enabled, err := h.cups.queueEnabled(ctx, queue)
	if err == nil && enabled {
		accepting, err := h.cups.queueAccepting(ctx, queue)
		healthy = err == nil && accepting
	}

	h.mu.Lock()
	h.entries[queue] = healthEntry{at: h.now(), healthy: healthy}
	h.mu.Unlock()
	return healthy
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
