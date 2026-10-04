package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"testing/quick"
	"time"
)

func newThrottledLogger(window time.Duration, clock *fakeClock) (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(newDedupeHandler(inner, newLogThrottle(window, clock.Now))), &buf
}

func TestDedupeHandlerSuppressesRepeatsAndCounts(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)}
	logger, buf := newThrottledLogger(time.Hour, clock)

	// The same mount at the same level, with drifting measurements, is one line.
	for i := 0; i < 10; i++ {
		logger.Info("disk status", "mount", "data", "free_gb", 20+float64(i)/10, "level", "critical")
		clock.Advance(5 * time.Minute)
	}
	// A second mount is a different line.
	logger.Info("disk status", "mount", "home", "free_gb", 50.0, "level", "none")
	if got := strings.Count(buf.String(), "disk status"); got != 2 {
		t.Fatalf("want 2 lines (one per mount), got %d:\n%s", got, buf.String())
	}

	// A level change is logged at once, with the count of held-back copies.
	logger.Info("disk status", "mount", "data", "free_gb", 21.0, "level", "aggressive")
	if !strings.Contains(buf.String(), "level=aggressive repeats_suppressed=9") {
		t.Fatalf("level change not logged with the held-back count:\n%s", buf.String())
	}

	// After the window an unchanged line is restated once.
	buf.Reset()
	clock.Advance(time.Hour)
	logger.Info("disk status", "mount", "data", "free_gb", 22.0, "level", "aggressive")
	logger.Info("disk status", "mount", "data", "free_gb", 22.0, "level", "aggressive")
	if got := strings.Count(buf.String(), "disk status"); got != 1 {
		t.Fatalf("want one restatement after the window, got %d:\n%s", got, buf.String())
	}

	// A new retry time is new content, so each one is logged once.
	buf.Reset()
	logger.Warn("plugin suppressed", "plugin", "dev-artifacts", "reason", "zero_yield", "retry_at", "2026-10-04T01:00:00Z")
	logger.Warn("plugin suppressed", "plugin", "dev-artifacts", "reason", "zero_yield", "retry_at", "2026-10-04T01:00:00Z")
	logger.Warn("plugin suppressed", "plugin", "dev-artifacts", "reason", "zero_yield", "retry_at", "2026-10-04T02:00:00Z")
	if got := strings.Count(buf.String(), "plugin suppressed"); got != 2 {
		t.Fatalf("want each retry_at once, got %d:\n%s", got, buf.String())
	}
	if !strings.Contains(buf.String(), "retry_at=2026-10-04T02:00:00Z") {
		t.Fatalf("new retry_at missing:\n%s", buf.String())
	}
}

func TestDedupeHandlerErrorWindowAndDebugPassThrough(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)}
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(newDedupeHandler(inner, newLogThrottle(time.Hour, clock.Now)))

	logger.Error("cleanup cycle failed", "error", "boom")
	clock.Advance(errorLogRepeatWindow)
	logger.Error("cleanup cycle failed", "error", "boom")
	if got := strings.Count(buf.String(), "cleanup cycle failed"); got != 2 {
		t.Fatalf("errors restate every %s, got %d lines", errorLogRepeatWindow, got)
	}
	logger.Debug("running plugins", "count", 3)
	logger.Debug("running plugins", "count", 3)
	if got := strings.Count(buf.String(), "running plugins"); got != 2 {
		t.Fatalf("debug lines pass through, got %d", got)
	}
}

func TestDedupeHandlerDisabledByZeroWindow(t *testing.T) {
	inner := slog.NewTextHandler(&bytes.Buffer{}, nil)
	if newDedupeHandler(inner, newLogThrottle(0, nil)) != inner {
		t.Fatal("a zero window must leave the handler unwrapped")
	}
	if got := logRepeatWindow("0s"); got != 0 {
		t.Fatalf("0s disables, got %s", got)
	}
	if got := logRepeatWindow(""); got != defaultLogRepeatWindow {
		t.Fatalf("empty uses the default, got %s", got)
	}
}

// throttleStep is one record for one key: which content it carries and how
// long after the previous record it arrives.
type throttleStep struct {
	Fingerprint uint8
	AdvanceMin  uint8
}

// replayThrottle feeds steps for a single key and returns emitted lines, the
// held-back copies reported on emitted lines, and the copies still pending.
func replayThrottle(steps []throttleStep, window time.Duration) (emitted, reported, pending, changes int) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	throttle := newLogThrottle(window, clock.Now)
	last := -1
	for _, step := range steps {
		clock.Advance(time.Duration(step.AdvanceMin%30) * time.Minute)
		fp := int(step.Fingerprint % 3)
		if fp != last {
			changes++
			last = fp
		}
		emit, held := throttle.decide("k", uint64(fp), slog.LevelInfo)
		if emit {
			emitted++
			reported += held
		}
	}
	if entry := throttle.entries["k"]; entry != nil {
		pending = entry.suppressed
	}
	return emitted, reported, pending, changes
}

// Property: no record is lost. Each one is emitted, counted on a later
// emitted line, or still pending.
func TestPropertyThrottleConservesRecords(t *testing.T) {
	prop := func(steps []throttleStep) bool {
		emitted, reported, pending, _ := replayThrottle(steps, time.Hour)
		return emitted+reported+pending == len(steps)
	}
	if err := quick.Check(prop, nil); err != nil {
		t.Fatal(err)
	}
}

// Property: emission is bounded by content changes plus one per window.
func TestPropertyThrottleBoundsEmission(t *testing.T) {
	prop := func(steps []throttleStep) bool {
		emitted, _, _, changes := replayThrottle(steps, time.Hour)
		var elapsed time.Duration
		for _, step := range steps {
			elapsed += time.Duration(step.AdvanceMin%30) * time.Minute
		}
		return emitted <= changes+int(elapsed/time.Hour)
	}
	if err := quick.Check(prop, nil); err != nil {
		t.Fatal(err)
	}
}

// Property: the throttle never holds more than maxKeys line identities.
func TestPropertyThrottleMemoryBounded(t *testing.T) {
	prop := func(keys []uint16) bool {
		clock := &fakeClock{t: time.Unix(0, 0)}
		throttle := newLogThrottle(time.Hour, clock.Now)
		throttle.maxKeys = 8
		for _, key := range keys {
			clock.Advance(time.Second)
			throttle.decide(string(rune(key)), 0, slog.LevelInfo)
			if len(throttle.entries) > throttle.maxKeys {
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, nil); err != nil {
		t.Fatal(err)
	}
}

func TestUnchangedReportCompactedWithRetryTime(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)}
	var out bytes.Buffer
	d := &daemon{output: "text", report: &out, now: clock.Now, reports: &reportThrottle{window: time.Hour}}
	report := cycleReport{
		Timestamp:           clock.Now().Format(time.RFC3339),
		Level:               "critical",
		ByteBackoff:         true,
		HostFreeBeforeBytes: 25 << 30,
		NextRetryAt:         "2026-10-04T00:30:00Z",
		NextCycleAt:         "2026-10-04T00:05:00Z",
		Plugins: []pluginCycleReport{{
			Name: "dev-artifacts", SkipReason: "zero_yield_backoff", RetryAt: "2026-10-04T00:30:00Z",
		}},
	}

	write := func() string {
		t.Helper()
		out.Reset()
		if err := d.writeDaemonReport(report); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	if got := write(); strings.HasPrefix(got, "cycle unchanged") {
		t.Fatalf("first report must be full, got %q", got)
	}
	clock.Advance(5 * time.Minute)
	report.HostFreeBeforeBytes = 24 << 30 // drift alone is not a change
	got := write()
	want := "cycle unchanged: timestamp=2026-10-04T00:00:00Z level=critical reason=byte_backoff,zero_yield_backoff free_gb=24.0 next_retry_at=2026-10-04T00:30:00Z next_cycle_at=2026-10-04T00:05:00Z unchanged_count=1\n"
	if got != want {
		t.Fatalf("compact line\n got %q\nwant %q", got, want)
	}

	// A new retry time is a change: the full report is written.
	report.NextRetryAt = "2026-10-04T01:30:00Z"
	if got := write(); strings.HasPrefix(got, "cycle unchanged") {
		t.Fatalf("changed retry time must write the full report, got %q", got)
	}
	// So is the window passing with nothing changed.
	clock.Advance(time.Hour)
	if got := write(); strings.HasPrefix(got, "cycle unchanged") {
		t.Fatalf("window elapsed must write the full report, got %q", got)
	}

	// JSON output compacts to one parseable line.
	d.output = "json"
	clock.Advance(time.Minute)
	if got := write(); !strings.HasPrefix(got, `{"report":"unchanged"`) || strings.Count(got, "\n") != 1 {
		t.Fatalf("json compact line, got %q", got)
	}
}

func TestOperatorRunsAlwaysWriteFullReport(t *testing.T) {
	var out bytes.Buffer
	d := &daemon{output: "text", report: &out}
	for i := 0; i < 3; i++ {
		if err := d.writeDaemonReport(cycleReport{Level: "critical"}); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(out.String(), "cycle unchanged") {
		t.Fatalf("without a report throttle every report is full:\n%s", out.String())
	}
}

func TestDedupeHandlerScopesWithAttrs(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	logger, buf := newThrottledLogger(time.Hour, clock)
	logger.With("plugin", "nix").Info("deferred")
	logger.With("plugin", "bazel").Info("deferred")
	logger.With("plugin", "nix").Info("deferred")
	if got := strings.Count(buf.String(), "deferred"); got != 2 {
		t.Fatalf("WithAttrs identity separates lines, got %d:\n%s", got, buf.String())
	}
}
