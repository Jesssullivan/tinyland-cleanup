package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/Jesssullivan/tinyland-cleanup/monitor"
	"github.com/Jesssullivan/tinyland-cleanup/plugins"
)

func writeTextReport(w io.Writer, report cycleReport) error {
	mode := "cleanup"
	if report.DryRun {
		mode = "dry-run"
	}
	if report.Level == monitor.LevelNone.String() {
		mode = "monitor"
	}

	if _, err := fmt.Fprintf(w, "tinyland-cleanup %s report\n", mode); err != nil {
		return err
	}
	if report.Timestamp != "" {
		if _, err := fmt.Fprintf(w, "time: %s\n", report.Timestamp); err != nil {
			return err
		}
	}

	levelLine := fmt.Sprintf("level: %s", report.Level)
	if report.ForcedLevel {
		levelLine += " (forced)"
	}
	if _, err := fmt.Fprintln(w, levelLine); err != nil {
		return err
	}

	if report.MonitorPath != "" {
		if _, err := fmt.Fprintf(w, "monitor: %s\n", report.MonitorPath); err != nil {
			return err
		}
	}
	if len(report.PluginFilter) > 0 {
		if _, err := fmt.Fprintf(w, "plugin filter: %s\n", strings.Join(report.PluginFilter, ", ")); err != nil {
			return err
		}
	}
	if report.HostFreeError != "" {
		if _, err := fmt.Fprintf(w, "host free: unavailable (%s)\n", report.HostFreeError); err != nil {
			return err
		}
	} else if report.HostFreeBeforeBytes > 0 || report.HostFreeAfterBytes > 0 {
		after := "not measured"
		if report.HostFreeAfterBytes > 0 {
			after = formatByteCount(int64(report.HostFreeAfterBytes))
		}
		if _, err := fmt.Fprintf(w, "host free: %s before, %s after, delta %s\n",
			formatByteCount(int64(report.HostFreeBeforeBytes)),
			after,
			formatSignedByteCount(report.HostFreeDeltaBytes),
		); err != nil {
			return err
		}
	}
	if report.HostInodesTotal > 0 {
		after := "not measured"
		afterUsed := report.HostInodesUsedPercentBefore
		if report.HostFreeAfterBytes > 0 || report.HostInodesFreeAfter > 0 || report.HostInodesUsedPercentAfter > 0 {
			after = formatCount(report.HostInodesFreeAfter)
			afterUsed = report.HostInodesUsedPercentAfter
		}
		if _, err := fmt.Fprintf(w, "host inodes: %.1f%% used before, %.1f%% used after, %s free before, %s free after, delta %s, level %s\n",
			report.HostInodesUsedPercentBefore,
			afterUsed,
			formatCount(report.HostInodesFreeBefore),
			after,
			formatSignedCount(report.HostInodesFreeDelta),
			report.HostInodeLevel,
		); err != nil {
			return err
		}
	}

	if report.TargetUsedPercent > 0 {
		if _, err := fmt.Fprintf(w, "target: <=%d%% used, need %s free, deficit %s\n",
			report.TargetUsedPercent,
			formatByteCount(int64(report.TargetFreeBytes)),
			formatByteCount(report.TargetFreeDeficitBytes),
		); err != nil {
			return err
		}
	}
	if report.MinimumFreeBytes > 0 {
		if _, err := fmt.Fprintf(w, "minimum free: need %s free, deficit %s\n",
			formatByteCount(int64(report.MinimumFreeBytes)),
			formatByteCount(report.MinimumFreeDeficitBytes),
		); err != nil {
			return err
		}
	}
	if report.StopReason != "" {
		if _, err := fmt.Fprintf(w, "stop: %s\n", report.StopReason); err != nil {
			return err
		}
	}
	if report.InodeBackoff {
		if _, err := fmt.Fprintf(w, "inode backoff: pressure unrelieved after %d cycles, applying cooldown\n",
			report.InodeNoProgressCount); err != nil {
			return err
		}
	}

	if len(report.Mounts) > 0 {
		if _, err := fmt.Fprintln(w, "mounts:"); err != nil {
			return err
		}
		for _, mount := range report.Mounts {
			label := mount.Label
			if label == "" {
				label = mount.Path
			}
			if mount.Error != "" {
				if _, err := fmt.Fprintf(w, "- %s (%s): unavailable (%s)\n", label, mount.Path, mount.Error); err != nil {
					return err
				}
				continue
			}
			inodeStatus := "inodes unavailable"
			if mount.InodesTotal > 0 {
				inodeStatus = fmt.Sprintf("%.1f%% inodes used, %s inodes free",
					mount.InodesUsedPercent,
					formatCount(mount.InodesFree))
			}
			if _, err := fmt.Fprintf(w, "- %s (%s): %.1f%% bytes used, %s free, %s, level %s",
				label,
				mount.Path,
				mount.UsedPercent,
				formatByteCount(int64(mount.FreeBytes)),
				inodeStatus,
				mount.Level,
			); err != nil {
				return err
			}
			if mount.ByteLevel != "" || mount.InodeLevel != "" {
				inodeLevel := mount.InodeLevel
				if inodeLevel == "" {
					inodeLevel = "n/a"
				}
				if _, err := fmt.Fprintf(w, " (bytes %s, inodes %s)", mount.ByteLevel, inodeLevel); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
	}

	if report.PlannedEstimatedBytesFreed > 0 || report.PlannedRequiredFreeBytes > 0 || report.PlannedTargets > 0 {
		if _, err := fmt.Fprintf(w, "plan: estimated reclaim %s, required free %s, targets %d\n",
			formatByteCount(report.PlannedEstimatedBytesFreed),
			formatByteCount(report.PlannedRequiredFreeBytes),
			report.PlannedTargets,
		); err != nil {
			return err
		}
	}
	if !report.DryRun && (report.TotalBytesFreed > 0 || report.TotalItemsCleaned > 0) {
		if _, err := fmt.Fprintf(w, "cleaned: %s across %d items\n",
			formatByteCount(report.TotalBytesFreed),
			report.TotalItemsCleaned,
		); err != nil {
			return err
		}
	}

	if len(report.Plugins) == 0 {
		return nil
	}

	if _, err := fmt.Fprintln(w, "plugins:"); err != nil {
		return err
	}
	for _, plugin := range report.Plugins {
		if err := writeTextPluginReport(w, plugin); err != nil {
			return err
		}
	}
	return nil
}

func writeTextPluginReport(w io.Writer, plugin pluginCycleReport) error {
	status := "would run"
	if !plugin.WouldRun {
		status = "skipped"
	}
	if plugin.SkipReason != "" {
		status += " (" + plugin.SkipReason + ")"
	}

	if _, err := fmt.Fprintf(w, "- %s: %s\n", plugin.Name, status); err != nil {
		return err
	}

	if plugin.Plan != nil {
		plan := plugin.Plan
		if plan.Summary != "" {
			if _, err := fmt.Fprintf(w, "  plan: %s\n", plan.Summary); err != nil {
				return err
			}
		}
		if !plan.WouldRun && plan.SkipReason != "" {
			if _, err := fmt.Fprintf(w, "  plan skip: %s\n", plan.SkipReason); err != nil {
				return err
			}
		}
		if plan.EstimatedBytesFreed > 0 || plan.RequiredFreeBytes > 0 || len(plan.Targets) > 0 {
			if _, err := fmt.Fprintf(w, "  estimate: %s reclaim, %s required free, %d targets\n",
				formatByteCount(plan.EstimatedBytesFreed),
				formatByteCount(plan.RequiredFreeBytes),
				len(plan.Targets),
			); err != nil {
				return err
			}
		}
		if err := writeTextPlanAccounting(w, plan.Metadata); err != nil {
			return err
		}
		if len(plan.Warnings) > 0 {
			if err := writeTextList(w, "  warnings:", plan.Warnings, 3); err != nil {
				return err
			}
		}
		if len(plan.Targets) > 0 {
			if _, err := fmt.Fprintln(w, "  targets:"); err != nil {
				return err
			}
			for idx, target := range plan.Targets {
				if idx >= 5 {
					if _, err := fmt.Fprintf(w, "  - ... %d more targets\n", len(plan.Targets)-idx); err != nil {
						return err
					}
					break
				}
				if err := writeTextTarget(w, target); err != nil {
					return err
				}
			}
		}
		return nil
	}

	if plugin.Error != "" {
		if _, err := fmt.Fprintf(w, "  error: %s\n", plugin.Error); err != nil {
			return err
		}
	}
	if plugin.CooldownRemainingSeconds > 0 {
		if _, err := fmt.Fprintf(w, "  cooldown remaining: %ds\n", plugin.CooldownRemainingSeconds); err != nil {
			return err
		}
	}
	if plugin.BytesFreed > 0 || plugin.ItemsCleaned > 0 {
		if _, err := fmt.Fprintf(w, "  cleaned: %s across %d items\n",
			formatByteCount(plugin.BytesFreed),
			plugin.ItemsCleaned,
		); err != nil {
			return err
		}
	}
	return nil
}

func writeTextPlanAccounting(w io.Writer, metadata map[string]string) error {
	total, ok := planMetadataInt64(metadata, "total_physical_bytes")
	if !ok || total <= 0 {
		return nil
	}
	hostReclaim, _ := planMetadataInt64(metadata, "host_reclaim_candidate_bytes")
	deferred, _ := planMetadataInt64(metadata, "deferred_reclaim_candidate_bytes")
	protected, _ := planMetadataInt64(metadata, "protected_physical_bytes")
	activeProtected, _ := planMetadataInt64(metadata, "active_protected_physical_bytes")
	review, _ := planMetadataInt64(metadata, "review_physical_bytes")

	_, err := fmt.Fprintf(w, "  accounting: total %s, host reclaim %s, deferred %s, protected %s, active protected %s, review %s\n",
		formatByteCount(total),
		formatByteCount(hostReclaim),
		formatByteCount(deferred),
		formatByteCount(protected),
		formatByteCount(activeProtected),
		formatByteCount(review),
	)
	return err
}

func planMetadataInt64(metadata map[string]string, key string) (int64, bool) {
	if len(metadata) == 0 {
		return 0, false
	}
	raw, ok := metadata[key]
	if !ok {
		return 0, false
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	return value, err == nil
}

func writeTextList(w io.Writer, header string, items []string, limit int) error {
	if _, err := fmt.Fprintln(w, header); err != nil {
		return err
	}
	for idx, item := range items {
		if idx >= limit {
			if _, err := fmt.Fprintf(w, "  - ... %d more\n", len(items)-idx); err != nil {
				return err
			}
			break
		}
		if _, err := fmt.Fprintf(w, "  - %s\n", item); err != nil {
			return err
		}
	}
	return nil
}

func writeTextTarget(w io.Writer, target plugins.CleanupTarget) error {
	status := target.Action
	if target.Protected {
		status += ", protected"
	}
	if target.Active {
		status += ", active"
	}

	name := target.Name
	if name == "" {
		name = target.Path
	}
	if name == "" {
		name = target.Type
	}
	if target.Path != "" && target.Path != name {
		name = fmt.Sprintf("%s (%s)", name, target.Path)
	}

	if _, err := fmt.Fprintf(w, "  - %s [%s]: %s", name, target.Type, status); err != nil {
		return err
	}
	if target.Tier != "" {
		if _, err := fmt.Fprintf(w, ", tier=%s", target.Tier); err != nil {
			return err
		}
	}
	if target.Reclaim != "" {
		if _, err := fmt.Fprintf(w, ", reclaim=%s", target.Reclaim); err != nil {
			return err
		}
	}
	if target.Bytes > 0 {
		if _, err := fmt.Fprintf(w, ", %s", formatByteCount(target.Bytes)); err != nil {
			return err
		}
	}
	if target.LogicalBytes > 0 && target.LogicalBytes != target.Bytes {
		if _, err := fmt.Fprintf(w, ", logical %s", formatByteCount(target.LogicalBytes)); err != nil {
			return err
		}
	}
	if target.Reason != "" {
		if _, err := fmt.Fprintf(w, " - %s", target.Reason); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

func formatSignedByteCount(bytes int64) string {
	if bytes < 0 {
		return "-" + formatByteCount(-bytes)
	}
	if bytes > 0 {
		return "+" + formatByteCount(bytes)
	}
	return "0 B"
}

func formatSignedCount(count int64) string {
	if count < 0 {
		return "-" + formatCount(uint64(-count))
	}
	if count > 0 {
		return "+" + formatCount(uint64(count))
	}
	return "0"
}

func formatCount(count uint64) string {
	value := float64(count)
	// Use SI-style magnitude suffixes for inode counts. "G" (not "B") denotes
	// billions so a count is never confused with a byte size in the same report.
	units := []string{"", "K", "M", "G", "T"}
	unit := 0
	for value >= 1000 && unit < len(units)-1 {
		value /= 1000
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%d", count)
	}
	return fmt.Sprintf("%.1f%s", value, units[unit])
}

func formatByteCount(bytes int64) string {
	if bytes <= 0 {
		return "0 B"
	}

	value := float64(bytes)
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	unit := 0
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%d B", bytes)
	}
	return fmt.Sprintf("%.1f %s", value, units[unit])
}
