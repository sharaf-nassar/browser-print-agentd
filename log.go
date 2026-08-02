package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxAgentLogSize  = 8 * 1024 * 1024
	agentLogArchives = 7
)

// agentLogger writes the agent's one-line-per-event log. Every line starts with
// a UTC RFC3339 timestamp so station logs collate with the SPA's own timeline,
// and every print job records the device uid, byte count, outcome, and the
// request Origin — the minimal audit trail v1 commits to.
type agentLogger struct {
	mu     sync.Mutex
	out    io.Writer
	closer io.Closer
	now    func() time.Time
}

// newAgentLogger writes to out.
func newAgentLogger(out io.Writer) *agentLogger {
	return &agentLogger{out: out, now: time.Now}
}

// newRotatingAgentLogger opens the packaged per-account log ring. The daemon,
// rather than launchd or the launcher, owns the descriptor so rotation can
// happen on the same concurrency boundary as complete record writes.
func newRotatingAgentLogger(path string) (*agentLogger, error) {
	out, err := newRotatingLog(path)
	if err != nil {
		return nil, err
	}
	return &agentLogger{out: out, closer: out, now: time.Now}, nil
}

// write emits one already-formatted line under the timestamp prefix.
func (l *agentLogger) write(line string) {
	if l == nil || l.out == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	record := []byte(fmt.Sprintf("%s %s\n",
		l.now().UTC().Format(time.RFC3339), line))
	n, err := l.out.Write(record)
	if err != nil || n != len(record) {
		if err == nil {
			err = io.ErrShortWrite
		}
		reportFallback(fmt.Sprintf("log write failed: %v", err))
	}
}

// close releases the packaged active descriptor after normal shutdown.
func (l *agentLogger) close() {
	if l == nil || l.closer == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.closer.Close(); err != nil {
		reportFallback(fmt.Sprintf("log close failed: %v", err))
	}
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

// rotatingLog retains one active file and seven uncompressed archives. Its
// Write method receives exactly one complete, timestamped record at a time
// while agentLogger.mu is held.
type rotatingLog struct {
	path string
	file *os.File
	size int64
}

func newRotatingLog(path string) (*rotatingLog, error) {
	if path == "" {
		return nil, fmt.Errorf("log path is empty")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("inspect log directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("log directory is not a real directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("make log directory private: %w", err)
	}

	r := &rotatingLog{path: path}
	if err := r.normalizeRing(); err != nil {
		return nil, err
	}
	if err := r.openActive(); err != nil {
		return nil, err
	}
	return r, nil
}

// normalizeRing removes only exact ring members that cannot fit the bound or
// are unsafe to open, and repairs permissions before the first intentional
// startup record. Unrelated files in the private directory are untouched.
func (r *rotatingLog) normalizeRing() error {
	for generation := 0; generation <= agentLogArchives; generation++ {
		path := r.path
		if generation > 0 {
			path += "." + strconv.Itoa(generation)
		}
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect log ring member: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
			info.Size() > maxAgentLogSize {
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("remove unsafe or oversize log ring member: %w", err)
			}
			continue
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("make log ring member private: %w", err)
		}
	}

	// Interrupted or predecessor implementations may have left numeric
	// generations outside the adopted .1-.7 ring. Remove only those precise
	// ring-shaped names; other files in the directory do not belong to us.
	dir := filepath.Dir(r.path)
	base := filepath.Base(r.path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read log directory: %w", err)
	}
	for _, entry := range entries {
		suffix, found := strings.CutPrefix(entry.Name(), base+".")
		if !found {
			continue
		}
		generation, err := strconv.Atoi(suffix)
		if err != nil || generation >= 1 && generation <= agentLogArchives {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return fmt.Errorf("remove out-of-ring log generation: %w", err)
		}
	}
	return nil
}

func (r *rotatingLog) openActive() error {
	file, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open active log: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return fmt.Errorf("make active log private: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return fmt.Errorf("inspect active log: %w", err)
	}
	if err := debug.SetCrashOutput(file, debug.CrashOptions{}); err != nil {
		file.Close()
		return fmt.Errorf("bind crash output to active log: %w", err)
	}
	r.file = file
	r.size = info.Size()
	return nil
}

func (r *rotatingLog) Write(record []byte) (int, error) {
	if r.file == nil {
		return 0, os.ErrClosed
	}
	if r.size > 0 && r.size+int64(len(record)) > maxAgentLogSize {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.file.Write(record)
	r.size += int64(n)
	return n, err
}

func (r *rotatingLog) rotate() error {
	if err := r.file.Close(); err != nil {
		return fmt.Errorf("close active log for rotation: %w", err)
	}
	r.file = nil

	oldest := r.path + "." + strconv.Itoa(agentLogArchives)
	if err := os.Remove(oldest); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove oldest log archive: %w", err)
	}
	for generation := agentLogArchives - 1; generation >= 1; generation-- {
		from := r.path + "." + strconv.Itoa(generation)
		to := r.path + "." + strconv.Itoa(generation+1)
		if err := os.Rename(from, to); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("shift log archive: %w", err)
		}
	}
	if err := os.Rename(r.path, r.path+".1"); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("archive active log: %w", err)
	}
	if err := r.openActive(); err != nil {
		return fmt.Errorf("create active log after rotation: %w", err)
	}
	return nil
}

func (r *rotatingLog) Close() error {
	// SetCrashOutput owns a duplicate of the active descriptor. Release it
	// before closing the writer so a normal shutdown leaves no stale inode.
	_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}
