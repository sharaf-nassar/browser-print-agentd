package main

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// agentLogger writes the agent's one-line-per-event log. Every line starts with
// a UTC RFC3339 timestamp so station logs collate with the SPA's own timeline,
// and every print job records the device uid, byte count, outcome, and the
// request Origin — the minimal audit trail v1 commits to.
type agentLogger struct {
	mu  sync.Mutex
	out io.Writer
	now func() time.Time
}

// newAgentLogger writes to out.
func newAgentLogger(out io.Writer) *agentLogger {
	return &agentLogger{out: out, now: time.Now}
}

// write emits one already-formatted line under the timestamp prefix.
func (l *agentLogger) write(line string) {
	if l == nil || l.out == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.out, "%s %s\n", l.now().UTC().Format(time.RFC3339), line)
}

// request records every inbound request with its Origin (Q14): the loopback
// surface is unauthenticated, so v1 makes abuse visible rather than silent.
func (l *agentLogger) request(method string, path string, origin string) {
	l.write(fmt.Sprintf("request method=%s path=%s origin=%s", method, path,
		originField(origin)))
}

// originRejected records a print request refused by the --origin-allow
// allowlist, before any lp call was made. action names the route so a rejected
// sheet and a rejected label are distinguishable in a station log.
func (l *agentLogger) originRejected(action string, origin string) {
	l.write(fmt.Sprintf("%s REJECTED reason=origin-not-allowed origin=%s",
		action, originField(origin)))
}

// fallback names the printer a job skipped. The silent-success failure mode is
// exactly what made the dead-USB bug invisible, so a failover is never quiet.
func (l *agentLogger) fallback(action string, requested string, target printer) {
	l.write(fmt.Sprintf("%s fallback requested=%s (unusable) -> uid=%s queue=%s",
		action, requested, target.UID, target.Queue))
}

// job records the outcome of a spooled print job.
func (l *agentLogger) job(
	action string, ok bool, byteCount int, target printer, requestID string, origin string,
) {
	status := "FAILED"
	if ok {
		status = "ok"
	}
	detail := requestID
	if detail == "" {
		detail = "-"
	}
	l.write(fmt.Sprintf(
		"%s %s bytes=%d uid=%s queue=%s connection=%s lp=%s origin=%s",
		action, status, byteCount, target.UID, target.Queue, target.Connection, detail,
		originField(origin)))
}

// originField renders an absent Origin header as "-" so the field is always
// present and greppable.
func originField(origin string) string {
	if origin == "" {
		return "-"
	}
	return origin
}
