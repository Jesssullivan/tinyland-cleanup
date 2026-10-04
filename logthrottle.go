package main

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// Bounded duplicate log emission (TIN-3342 criterion 5).
//
// A daemon pinned under pressure repeats the same lines every poll: the
// "disk status" line per mount, the free-space line, the same plugin warning,
// and the full cycle report. The throttle below emits a line when its content
// changes and otherwise at most once per repeat window, carrying a count of
// the copies it held back. Measurements that drift every cycle (free bytes,
// percentages) do not count as a change, so a steady state is quiet while a
// level change, a new reason or a new retry time is logged at once.

const (
	defaultLogRepeatWindow = time.Hour
	// errorLogRepeatWindow caps the window for Error records so a persistent
	// failure is restated more often than routine status.
	errorLogRepeatWindow = 15 * time.Minute
	// maxThrottleKeys bounds the throttle's memory; the oldest key is evicted
	// when a new one would exceed it.
	maxThrottleKeys = 1024
)

// throttleIdentityKeys name the attributes that make two records different
// lines rather than repeats of one line (one per mount, plugin or path).
var throttleIdentityKeys = map[string]bool{
	"plugin":   true,
	"path":     true,
	"mount":    true,
	"label":    true,
	"instance": true,
}

// throttleVolatileKeys are measurements that drift between cycles without
// the line saying anything new; they never count as a content change.
var throttleVolatileKeys = map[string]bool{
	"used_percent":        true,
	"free_gb":             true,
	"free_bytes":          true,
	"inodes_used_percent": true,
	"inodes_free":         true,
	"before_free_gb":      true,
	"after_free_gb":       true,
	"delta_mb":            true,
	"cycle_duration":      true,
	"repeats_suppressed":  true,
}

type throttleEntry struct {
	fingerprint uint64
	lastEmit    time.Time
	suppressed  int
}

// logThrottle decides, per line identity, whether a record is emitted. It is
// safe for concurrent use and holds at most maxKeys entries.
type logThrottle struct {
	mu      sync.Mutex
	window  time.Duration
	now     func() time.Time
	maxKeys int
	entries map[string]*throttleEntry
}

func newLogThrottle(window time.Duration, now func() time.Time) *logThrottle {
	if now == nil {
		now = time.Now
	}
	return &logThrottle{window: window, now: now, maxKeys: maxThrottleKeys, entries: map[string]*throttleEntry{}}
}

func (t *logThrottle) windowFor(level slog.Level) time.Duration {
	if level >= slog.LevelError && errorLogRepeatWindow < t.window {
		return errorLogRepeatWindow
	}
	return t.window
}

// decide reports whether the record is emitted and, if so, how many earlier
// copies were held back since the last emitted line for this key.
func (t *logThrottle) decide(key string, fingerprint uint64, level slog.Level) (bool, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	entry, ok := t.entries[key]
	if !ok {
		t.evictLocked()
		t.entries[key] = &throttleEntry{fingerprint: fingerprint, lastEmit: now}
		return true, 0
	}
	if entry.fingerprint != fingerprint || now.Sub(entry.lastEmit) >= t.windowFor(level) {
		held := entry.suppressed
		entry.fingerprint = fingerprint
		entry.lastEmit = now
		entry.suppressed = 0
		return true, held
	}
	entry.suppressed++
	return false, 0
}

func (t *logThrottle) evictLocked() {
	if len(t.entries) < t.maxKeys {
		return
	}
	oldestKey := ""
	var oldest time.Time
	for key, entry := range t.entries {
		if oldestKey == "" || entry.lastEmit.Before(oldest) {
			oldestKey, oldest = key, entry.lastEmit
		}
	}
	delete(t.entries, oldestKey)
}

// dedupeHandler wraps a slog.Handler with a logThrottle. Debug records pass
// through untouched: they are only visible with -verbose, where an operator
// asked for every line.
type dedupeHandler struct {
	inner    slog.Handler
	throttle *logThrottle
	scope    string
}

func newDedupeHandler(inner slog.Handler, throttle *logThrottle) slog.Handler {
	if throttle == nil || throttle.window <= 0 {
		return inner
	}
	return &dedupeHandler{inner: inner, throttle: throttle}
}

func (h *dedupeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *dedupeHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Level < slog.LevelInfo {
		return h.inner.Handle(ctx, record)
	}
	key, fingerprint := recordIdentity(h.scope, record)
	emit, held := h.throttle.decide(key, fingerprint, record.Level)
	if !emit {
		return nil
	}
	if held > 0 {
		record = record.Clone()
		record.AddAttrs(slog.Int("repeats_suppressed", held))
	}
	return h.inner.Handle(ctx, record)
}

func (h *dedupeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	var b strings.Builder
	b.WriteString(h.scope)
	for _, attr := range attrs {
		fmt.Fprintf(&b, "|%s=%s", attr.Key, attr.Value.Resolve().String())
	}
	return &dedupeHandler{inner: h.inner.WithAttrs(attrs), throttle: h.throttle, scope: b.String()}
}

func (h *dedupeHandler) WithGroup(name string) slog.Handler {
	return &dedupeHandler{inner: h.inner.WithGroup(name), throttle: h.throttle, scope: h.scope + "|g:" + name}
}

// recordIdentity returns the line identity (level, message, scope and
// identity attributes) and a fingerprint of the remaining non-volatile
// attributes.
func recordIdentity(scope string, record slog.Record) (string, uint64) {
	var key strings.Builder
	fmt.Fprintf(&key, "%s|%s%s", record.Level, record.Message, scope)
	hash := fnv.New64a()
	var walk func(prefix string, attr slog.Attr)
	walk = func(prefix string, attr slog.Attr) {
		value := attr.Value.Resolve()
		name := prefix + attr.Key
		if value.Kind() == slog.KindGroup {
			for _, child := range value.Group() {
				walk(name+".", child)
			}
			return
		}
		switch {
		case throttleIdentityKeys[name]:
			fmt.Fprintf(&key, "|%s=%s", name, value.String())
		case throttleVolatileKeys[name]:
		default:
			fmt.Fprintf(hash, "%s=%s;", name, value.String())
		}
	}
	record.Attrs(func(attr slog.Attr) bool {
		walk("", attr)
		return true
	})
	return key.String(), hash.Sum64()
}

// logRepeatWindow returns policy.log_repeat_window. Empty or invalid uses the
// default; zero or negative disables throttling.
func logRepeatWindow(raw string) time.Duration {
	if strings.TrimSpace(raw) == "" {
		return defaultLogRepeatWindow
	}
	window, err := time.ParseDuration(raw)
	if err != nil {
		return defaultLogRepeatWindow
	}
	return window
}

// reportThrottle compacts the daemon's per-cycle report: an unchanged cycle
// writes one line instead of the full report, and the full report is written
// on any change and at least once per window.
type reportThrottle struct {
	window      time.Duration
	fingerprint string
	lastFull    time.Time
	unchanged   int
}

// full reports whether this cycle's report should be written in full.
func (r *reportThrottle) full(report cycleReport, now time.Time) bool {
	fingerprint := reportFingerprint(report)
	if r.window <= 0 || r.lastFull.IsZero() || fingerprint != r.fingerprint || now.Sub(r.lastFull) >= r.window {
		r.fingerprint = fingerprint
		r.lastFull = now
		r.unchanged = 0
		return true
	}
	r.unchanged++
	return false
}

// reportFingerprint covers what an operator acts on: levels, reasons,
// backoff state, retry times and per-plugin outcomes. Free-space readings are
// left out; the compact line still carries the current value.
func reportFingerprint(report cycleReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|%s|%s|%s|%t|%s|%t|%s|%s|%s",
		report.Level, report.HostByteLevel, report.HostInodeLevel, report.MaxInodeLevel,
		report.StopReason, report.ByteBackoff, report.ByteBackoffReason, report.InodeBackoff,
		report.StateError, report.StateQuarantined, report.NextRetryAt)
	for _, mount := range report.Mounts {
		fmt.Fprintf(&b, "|m:%s:%s:%s", mount.Label, mount.Level, mount.Error)
	}
	for _, plugin := range report.Plugins {
		fmt.Fprintf(&b, "|p:%s:%t:%t:%s:%s:%s:%s:%d:%d:%s",
			plugin.Name, plugin.Ran, plugin.WouldRun, plugin.SkipReason, plugin.RetryAt,
			plugin.SuppressedUntil, plugin.ZeroYieldLifted, plugin.BytesFreed, plugin.ItemsCleaned, plugin.Error)
	}
	return b.String()
}

// unchangedReason summarizes why an unchanged cycle did nothing new.
func unchangedReason(report cycleReport) string {
	if report.StopReason != "" {
		return report.StopReason
	}
	if report.Level == "none" || report.Level == "" {
		return "no_pressure"
	}
	seen := map[string]bool{}
	for _, plugin := range report.Plugins {
		if plugin.SkipReason != "" {
			seen[plugin.SkipReason] = true
		}
	}
	if report.ByteBackoff {
		seen["byte_backoff"] = true
	}
	if len(seen) == 0 {
		return "no_change"
	}
	reasons := make([]string, 0, len(seen))
	for reason := range seen {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	return strings.Join(reasons, ",")
}

type compactReport struct {
	Report         string `json:"report"`
	Timestamp      string `json:"timestamp"`
	Level          string `json:"level"`
	Reason         string `json:"reason"`
	FreeGB         string `json:"free_gb"`
	NextRetryAt    string `json:"next_retry_at,omitempty"`
	NextCycleAt    string `json:"next_cycle_at,omitempty"`
	UnchangedCount int    `json:"unchanged_count"`
}

// writeCompactReport writes the one-line form of an unchanged cycle.
func writeCompactReport(w io.Writer, output string, report cycleReport, unchanged int) error {
	line := compactReport{
		Report:         "unchanged",
		Timestamp:      report.Timestamp,
		Level:          report.Level,
		Reason:         unchangedReason(report),
		FreeGB:         bytesToGB(report.HostFreeBeforeBytes),
		NextRetryAt:    report.NextRetryAt,
		NextCycleAt:    report.NextCycleAt,
		UnchangedCount: unchanged,
	}
	switch output {
	case "json":
		return json.NewEncoder(w).Encode(line)
	case "text":
		_, err := fmt.Fprintf(w, "cycle unchanged: timestamp=%s level=%s reason=%s free_gb=%s next_retry_at=%s next_cycle_at=%s unchanged_count=%d\n",
			line.Timestamp, line.Level, line.Reason, line.FreeGB, orDash(line.NextRetryAt), orDash(line.NextCycleAt), line.UnchangedCount)
		return err
	}
	return nil
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
