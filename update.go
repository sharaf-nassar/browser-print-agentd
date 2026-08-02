package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const (
	maxUpdateStatusBytes = 1024
	maxLaunchctlOutput   = 64 << 10
	launchctlBudget      = 500 * time.Millisecond
)

// updateReport is the sanitized root-updater state exposed by GET /health.
// LatestVersion and Quarantined are absent until a manifest has passed strict
// validation; guessing either from the installed receipt would be misleading.
type updateReport struct {
	LastCheck     string `json:"lastCheck"`
	Status        string `json:"status"`
	LatestVersion string `json:"latestVersion,omitempty"`
	Pinned        bool   `json:"pinned"`
	Quarantined   *bool  `json:"quarantined,omitempty"`
}

type publishedUpdateStatus struct {
	lastCheck     string
	status        string
	latestVersion string
	quarantined   *bool
}

type updateReader struct {
	path  string
	label string
}

// read combines the updater's sanitized publication with launchd's live pin
// truth. The updater cannot publish a durable pin boolean itself: disabling the
// job prevents it from running again to rewrite that boolean.
func (u *updateReader) read(ctx context.Context) *updateReport {
	published, err := readPublishedUpdateStatus(u.path)
	if err != nil {
		return nil
	}
	pinned, err := readLaunchdPin(ctx, u.label)
	if err != nil {
		return nil
	}
	return &updateReport{
		LastCheck:     published.lastCheck,
		Status:        published.status,
		LatestVersion: published.latestVersion,
		Pinned:        pinned,
		Quarantined:   published.quarantined,
	}
}

// readPublishedUpdateStatus accepts exactly the four newline-terminated
// records the root updater writes. Ownership, mode, type, size, grammar, and
// timestamp are all checked before any value reaches JSON.
func readPublishedUpdateStatus(path string) (publishedUpdateStatus, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm() != 0o644 {
		return publishedUpdateStatus{}, errors.New("unsafe updater status file")
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return publishedUpdateStatus{}, errors.New("updater status is not root-owned")
	}

	file, err := os.Open(path)
	if err != nil {
		return publishedUpdateStatus{}, err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		return publishedUpdateStatus{}, errors.New("updater status changed while opening")
	}
	body, err := io.ReadAll(io.LimitReader(file, maxUpdateStatusBytes+1))
	if err != nil || len(body) > maxUpdateStatusBytes {
		return publishedUpdateStatus{}, errors.New("updater status is too large")
	}
	return parsePublishedUpdateStatus(string(body))
}

func parsePublishedUpdateStatus(body string) (publishedUpdateStatus, error) {
	if strings.ContainsAny(body, "\x00\r") {
		return publishedUpdateStatus{}, errors.New("invalid updater status bytes")
	}
	lines := strings.Split(body, "\n")
	if len(lines) != 5 || lines[4] != "" {
		return publishedUpdateStatus{}, errors.New("invalid updater status record count")
	}
	const (
		timestampPrefix  = "timestamp="
		statusPrefix     = "status="
		versionPrefix    = "latest-version="
		quarantinePrefix = "latest-quarantined="
	)
	if !strings.HasPrefix(lines[0], timestampPrefix) ||
		!strings.HasPrefix(lines[1], statusPrefix) ||
		!strings.HasPrefix(lines[2], versionPrefix) ||
		!strings.HasPrefix(lines[3], quarantinePrefix) {
		return publishedUpdateStatus{}, errors.New("invalid updater status keys")
	}

	timestamp := strings.TrimPrefix(lines[0], timestampPrefix)
	parsedTime, err := time.Parse(time.RFC3339, timestamp)
	if err != nil || timestamp != parsedTime.UTC().Format(time.RFC3339) {
		return publishedUpdateStatus{}, errors.New("invalid updater timestamp")
	}
	status := strings.TrimPrefix(lines[1], statusPrefix)
	if !validUpdateStatus(status) {
		return publishedUpdateStatus{}, errors.New("invalid updater outcome")
	}
	latestVersion := strings.TrimPrefix(lines[2], versionPrefix)
	if latestVersion != "" && !validReleaseVersion(latestVersion) {
		return publishedUpdateStatus{}, errors.New("invalid latest version")
	}

	quarantine := strings.TrimPrefix(lines[3], quarantinePrefix)
	var quarantined *bool
	switch {
	case latestVersion == "" && quarantine == "unknown":
	case latestVersion != "" && quarantine == "true":
		value := true
		quarantined = &value
	case latestVersion != "" && quarantine == "false":
		value := false
		quarantined = &value
	default:
		return publishedUpdateStatus{}, errors.New("invalid quarantine state")
	}

	return publishedUpdateStatus{
		lastCheck:     timestamp,
		status:        status,
		latestVersion: latestVersion,
		quarantined:   quarantined,
	}, nil
}

func validReleaseVersion(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, character := range part {
			if character < '0' || character > '9' {
				return false
			}
		}
	}
	return true
}

func validUpdateStatus(value string) bool {
	switch value {
	case "skipped-install", "skipped-no-user", "manifest-fetch-failed",
		"manifest-invalid", "current", "quarantined",
		"rollback-cache-failed", "package-fetch-failed", "checksum-failed",
		"trust-failed", "updated", "install-failed", "rolled-back",
		"rollback-failed":
		return true
	default:
		return false
	}
}

// readLaunchdPin treats a complete, successful print-disabled dictionary as
// authoritative. An absent label means no disabled override. Duplicate or
// contradictory records, timeout, truncation, or command failure are unknown,
// so callers omit the entire update object instead of fabricating a boolean.
func readLaunchdPin(ctx context.Context, label string) (bool, error) {
	if !validLaunchdLabel(label) {
		return false, errors.New("invalid updater launchd label")
	}
	commandCtx, cancel := context.WithTimeout(ctx, launchctlBudget)
	defer cancel()
	var output boundedBuffer
	output.limit = maxLaunchctlOutput
	command := exec.CommandContext(commandCtx, "/bin/launchctl", "print-disabled", "system")
	command.Stdout = &output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil || commandCtx.Err() != nil || output.overflow {
		return false, errors.New("could not read bounded launchd disabled state")
	}
	return parseLaunchdPin(output.String(), label)
}

func parseLaunchdPin(output string, label string) (bool, error) {
	disabledLine := fmt.Sprintf("%q => disabled", label)
	enabledLine := fmt.Sprintf("%q => enabled", label)
	disabledCount := 0
	enabledCount := 0
	lines := make([]string, 0, 8)
	for _, line := range strings.Split(output, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	if len(lines) < 2 || lines[0] != "disabled services = {" || lines[len(lines)-1] != "}" {
		return false, errors.New("invalid launchd disabled-state dictionary")
	}
	for _, line := range lines[1 : len(lines)-1] {
		entry, state, found := strings.Cut(line, "\" => ")
		if !found || !strings.HasPrefix(entry, "\"") || len(entry) == 1 ||
			(state != "disabled" && state != "enabled") {
			return false, errors.New("invalid launchd disabled-state entry")
		}
		switch line {
		case disabledLine:
			disabledCount++
		case enabledLine:
			enabledCount++
		}
	}
	if disabledCount > 1 || enabledCount > 1 || disabledCount+enabledCount > 1 {
		return false, errors.New("ambiguous launchd disabled state")
	}
	return disabledCount == 1, nil
}

func validLaunchdLabel(label string) bool {
	if len(label) == 0 || len(label) > 255 {
		return false
	}
	for _, character := range label {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '.' || character == '-' {
			continue
		}
		return false
	}
	return true
}

type boundedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(chunk []byte) (int, error) {
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		b.overflow = true
		return len(chunk), nil
	}
	if len(chunk) > remaining {
		b.Buffer.Write(chunk[:remaining])
		b.overflow = true
		return len(chunk), nil
	}
	return b.Buffer.Write(chunk)
}
