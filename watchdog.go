package main

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
	"github.com/Jesssullivan/tinyland-cleanup/plugins"
)

// The plugin watchdog. Plugins used to run inline in the cycle loop, so one
// plugin that never returned stopped every plugin after it, every later cycle,
// and the report that would have said so. The service manager saw a live
// process and called it healthy.
//
// Each plugin now runs in its own goroutine under a deadline:
//
//  1. At the deadline the plugin's context ends, which terminates any command
//     it started (see plugins/exec_deadline.go).
//  2. The plugin then has a grace period to return. If it does, its result is
//     kept and the report says it was cancelled.
//  3. If it does not (stuck in a system call no context can interrupt), the
//     cycle moves on without it. The plugin stays marked in flight and is
//     skipped, with skip_reason "plugin_wedged", until that run returns, so
//     the same plugin never runs twice at once.

const (
	// defaultPluginTimeout is the watchdog deadline for one plugin run. It is
	// far above any healthy run: Nix collection is the longest, at 20m by
	// default.
	defaultPluginTimeout = 45 * time.Minute
	// defaultPluginGrace is how long a plugin has to return after its
	// deadline before the cycle moves on without it.
	defaultPluginGrace = 30 * time.Second
	// defaultMemoryLimitMB is the Go runtime soft memory limit the daemon
	// sets for itself when neither the config nor GOMEMLIMIT gives one.
	defaultMemoryLimitMB = 256

	skipReasonPluginWedged = "plugin_wedged"

	wedgeCancelled = "deadline_cancelled"
	wedgeAbandoned = "deadline_abandoned"
)

// pluginFlight is one plugin run the watchdog gave up waiting for.
type pluginFlight struct {
	started time.Time
	done    chan struct{}
	scope   *plugins.RunScope
}

// pluginWatch is what the watchdog observed about one plugin run.
type pluginWatch struct {
	// state is "" for a run that finished inside its deadline, otherwise
	// wedgeCancelled, wedgeAbandoned or skipReasonPluginWedged.
	state string
	// since is when the wedged run started.
	since time.Time
	// timeout is the deadline that applied.
	timeout time.Duration
	// lastCommand is the external command most recently started by the run.
	lastCommand string
	// commands lists "wedged:<command>" for each command killed at its deadline.
	commands []string
}

// returned reports whether the plugin function returned a result.
func (w pluginWatch) returned() bool {
	return w.state == "" || w.state == wedgeCancelled
}

func parsePolicyDuration(raw string, fallback time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	duration, err := config.ParseDuration(raw)
	if err != nil || duration <= 0 {
		return fallback
	}
	return duration
}

// pluginTimeout returns the watchdog deadline for one plugin.
func (d *daemon) pluginTimeout(name string) time.Duration {
	if d.config == nil {
		return defaultPluginTimeout
	}
	fallback := parsePolicyDuration(d.config.Policy.PluginTimeout, defaultPluginTimeout)
	if raw, ok := d.config.Policy.PluginTimeouts[name]; ok {
		return parsePolicyDuration(raw, fallback)
	}
	return fallback
}

func (d *daemon) pluginGraceDuration() time.Duration {
	if d.pluginGrace > 0 {
		return d.pluginGrace
	}
	return defaultPluginGrace
}

func (d *daemon) execTimeouts(name string) (probe, reclaim time.Duration) {
	probe, reclaim = plugins.DefaultExecTimeout, plugins.DefaultExecReclaimTimeout
	if d.config == nil {
		return probe, reclaim
	}
	probe = parsePolicyDuration(d.config.Policy.ExecTimeout, probe)
	reclaim = parsePolicyDuration(d.config.Policy.ExecReclaimTimeout, reclaim)
	if raw, ok := d.config.Policy.ExecTimeouts[name]; ok {
		probe = parsePolicyDuration(raw, probe)
	}
	return probe, reclaim
}

// watchPlugin runs fn, one plugin's Cleanup or PlanCleanup, under the
// watchdog. When watch.returned() is false, fn did not return in time or was
// not started, and result is the zero T.
func watchPlugin[T any](d *daemon, ctx context.Context, p plugins.Plugin, fn func(context.Context) T) (result T, watch pluginWatch) {
	name := p.Name()
	if flight := d.inFlight[name]; flight != nil {
		select {
		case <-flight.done:
			delete(d.inFlight, name)
			d.logger.Info("wedged plugin run returned; the plugin is eligible again",
				"plugin", name, "ran_for", time.Since(flight.started).Round(time.Second).String())
		default:
			last, _ := flight.scope.LastCommand()
			return result, pluginWatch{
				state:       skipReasonPluginWedged,
				since:       flight.started,
				lastCommand: last,
				commands:    flight.scope.Wedged(),
			}
		}
	}

	timeout := d.pluginTimeout(name)
	started := time.Now()
	deadline := started.Add(timeout)
	probe, reclaim := d.execTimeouts(name)
	scope := plugins.NewRunScope(name, deadline, probe, reclaim)
	pctx, cancel := context.WithDeadline(plugins.WithRunScope(ctx, scope), deadline)
	defer cancel()

	out := make(chan T, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		out <- fn(pctx)
	}()

	watch = pluginWatch{since: started, timeout: timeout}
	select {
	case <-done:
		watch.commands = scope.Wedged()
		return <-out, watch
	case <-pctx.Done():
	}

	grace := time.NewTimer(d.pluginGraceDuration())
	defer grace.Stop()
	select {
	case <-done:
		watch.commands = scope.Wedged()
		watch.lastCommand, _ = scope.LastCommand()
		if ctx.Err() == nil {
			// The deadline fired, not a shutdown.
			watch.state = wedgeCancelled
		}
		return <-out, watch
	case <-grace.C:
	}

	if d.inFlight == nil {
		d.inFlight = make(map[string]*pluginFlight)
	}
	d.inFlight[name] = &pluginFlight{started: started, done: done, scope: scope}
	watch.state = wedgeAbandoned
	watch.commands = scope.Wedged()
	watch.lastCommand, _ = scope.LastCommand()
	return result, watch
}

// describe is the operator-facing sentence for a wedged plugin run.
func (w pluginWatch) describe(name string) string {
	var b strings.Builder
	switch w.state {
	case wedgeCancelled:
		fmt.Fprintf(&b, "plugin %s exceeded its %s deadline and was cancelled", name, w.timeout)
	case wedgeAbandoned:
		fmt.Fprintf(&b, "plugin %s exceeded its %s deadline and did not return; the cycle continued without it", name, w.timeout)
	case skipReasonPluginWedged:
		fmt.Fprintf(&b, "plugin %s is still wedged in a run started at %s", name, w.since.UTC().Format(time.RFC3339))
	default:
		return ""
	}
	if w.lastCommand != "" {
		fmt.Fprintf(&b, " (last command: %s)", w.lastCommand)
	}
	return b.String()
}

// applyWatch copies what the watchdog saw into the plugin's report and logs
// it. It returns true when the plugin produced no result this cycle.
func (d *daemon) applyWatch(pluginReport *pluginCycleReport, watch pluginWatch) (noResult bool) {
	pluginReport.WedgedCommands = watch.commands
	for _, command := range watch.commands {
		d.logger.Warn("external command killed at its deadline", "plugin", pluginReport.Name, "command", strings.TrimPrefix(command, "wedged:"))
	}
	if watch.state == "" {
		return false
	}
	pluginReport.Wedged = watch.state
	pluginReport.LastCommand = watch.lastCommand
	message := watch.describe(pluginReport.Name)
	switch watch.state {
	case skipReasonPluginWedged:
		pluginReport.WouldRun = false
		pluginReport.SkipReason = skipReasonPluginWedged
		pluginReport.WedgedSince = watch.since.UTC().Format(time.RFC3339)
		d.logger.Warn(message, "plugin", pluginReport.Name)
		return true
	case wedgeAbandoned:
		pluginReport.WedgedSince = watch.since.UTC().Format(time.RFC3339)
		pluginReport.Error = message
		d.logger.Error(message, "plugin", pluginReport.Name)
		return true
	default:
		d.logger.Error(message, "plugin", pluginReport.Name)
		return false
	}
}

// applyMemoryLimit sets the Go runtime soft memory limit so the heap is
// collected well before a service memory cap is reached. GOMEMLIMIT in the
// environment wins; a negative policy.memory_limit_mb leaves the runtime
// default (no limit).
func applyMemoryLimit(cfg *config.Config, getenv func(string) string) int64 {
	if getenv("GOMEMLIMIT") != "" {
		return 0
	}
	limitMB := defaultMemoryLimitMB
	if cfg != nil && cfg.Policy.MemoryLimitMB != 0 {
		limitMB = cfg.Policy.MemoryLimitMB
	}
	if limitMB < 0 {
		return 0
	}
	limit := int64(limitMB) * 1024 * 1024
	debug.SetMemoryLimit(limit)
	return limit
}
