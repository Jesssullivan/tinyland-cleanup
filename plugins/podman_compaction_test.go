package plugins

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

func TestPodmanCompactionPlanUsesPhysicalAllocationForAppleHVRaw(t *testing.T) {
	cfg := testPodmanCompactionConfig()
	cfg.CompactProviderAllowlist = []string{"applehv"}

	plan := buildPodmanCompactionPlan(podmanCompactionPlanInput{
		MachineName:      "podman-machine-default",
		Provider:         "applehv",
		DiskPath:         "/Users/test/.local/share/containers/podman/machine/applehv/podman-machine-default.raw",
		ConfigEnabled:    true,
		QemuImgAvailable: true,
		DiskPathExpected: true,
		LogicalBytes:     100 * podmanCompactionGiB,
		PhysicalBytes:    12 * podmanCompactionGiB,
		FreeBytes:        14 * podmanCompactionGiB,
		Config:           cfg,
	})

	if !plan.CanCompact {
		t.Fatalf("expected compaction to be eligible, got skip reason %q", plan.SkipReason)
	}
	if plan.DiskFormat != "raw" {
		t.Fatalf("expected raw disk format, got %q", plan.DiskFormat)
	}
	if plan.RequiredFreeBytes >= plan.LogicalBytes {
		t.Fatalf("required free space should use physical allocation, required=%d logical=%d", plan.RequiredFreeBytes, plan.LogicalBytes)
	}
	if plan.RequiredFreeBytes <= plan.PhysicalBytes {
		t.Fatalf("required free space should include headroom, required=%d physical=%d", plan.RequiredFreeBytes, plan.PhysicalBytes)
	}
}

func TestPodmanCompactionPlanSupportsQemuQCow2(t *testing.T) {
	cfg := testPodmanCompactionConfig()
	cfg.CompactProviderAllowlist = []string{"qemu"}

	plan := buildPodmanCompactionPlan(podmanCompactionPlanInput{
		MachineName:      "podman-machine-default",
		Provider:         "qemu",
		DiskPath:         "/Users/test/.local/share/containers/podman/machine/qemu/podman-machine-default.qcow2",
		ConfigEnabled:    true,
		QemuImgAvailable: true,
		DiskPathExpected: true,
		LogicalBytes:     30 * podmanCompactionGiB,
		PhysicalBytes:    20 * podmanCompactionGiB,
		FreeBytes:        24 * podmanCompactionGiB,
		Config:           cfg,
	})

	if !plan.CanCompact {
		t.Fatalf("expected compaction to be eligible, got skip reason %q", plan.SkipReason)
	}
	if plan.DiskFormat != "qcow2" {
		t.Fatalf("expected qcow2 disk format, got %q", plan.DiskFormat)
	}
}

func TestPodmanCompactionPlanInsufficientFreeSpace(t *testing.T) {
	cfg := testPodmanCompactionConfig()

	plan := buildPodmanCompactionPlan(podmanCompactionPlanInput{
		MachineName:      "podman-machine-default",
		Provider:         "applehv",
		DiskPath:         "/Users/test/.local/share/containers/podman/machine/applehv/podman-machine-default.raw",
		ConfigEnabled:    true,
		QemuImgAvailable: true,
		DiskPathExpected: true,
		LogicalBytes:     100 * podmanCompactionGiB,
		PhysicalBytes:    12 * podmanCompactionGiB,
		FreeBytes:        4 * podmanCompactionGiB,
		Config:           cfg,
	})

	if plan.CanCompact {
		t.Fatal("expected insufficient free space to block compaction")
	}
	if plan.SkipReason != "insufficient_free_space" {
		t.Fatalf("expected insufficient_free_space, got %q", plan.SkipReason)
	}
}

func TestPodmanCompactionPlanRejectsUnknownProvider(t *testing.T) {
	cfg := testPodmanCompactionConfig()

	plan := buildPodmanCompactionPlan(podmanCompactionPlanInput{
		MachineName:      "podman-machine-default",
		Provider:         "mystery",
		DiskPath:         "/Users/test/.local/share/containers/podman/machine/mystery/podman-machine-default.raw",
		ConfigEnabled:    true,
		QemuImgAvailable: true,
		DiskPathExpected: true,
		LogicalBytes:     20 * podmanCompactionGiB,
		PhysicalBytes:    12 * podmanCompactionGiB,
		FreeBytes:        20 * podmanCompactionGiB,
		Config:           cfg,
	})

	if plan.CanCompact {
		t.Fatal("expected unknown provider to block compaction")
	}
	if plan.SkipReason != "unsupported_provider" {
		t.Fatalf("expected unsupported_provider, got %q", plan.SkipReason)
	}
}

func TestPodmanCompactionPlanRejectsActiveContainers(t *testing.T) {
	cfg := testPodmanCompactionConfig()
	cfg.CompactRequireNoActiveContainers = true

	plan := buildPodmanCompactionPlan(podmanCompactionPlanInput{
		MachineName:      "podman-machine-default",
		Provider:         "applehv",
		DiskPath:         "/Users/test/.local/share/containers/podman/machine/applehv/podman-machine-default.raw",
		ConfigEnabled:    true,
		QemuImgAvailable: true,
		ActiveContainers: true,
		DiskPathExpected: true,
		LogicalBytes:     100 * podmanCompactionGiB,
		PhysicalBytes:    12 * podmanCompactionGiB,
		FreeBytes:        20 * podmanCompactionGiB,
		Config:           cfg,
	})

	if plan.CanCompact {
		t.Fatal("expected active containers to block compaction")
	}
	if plan.SkipReason != "active_containers" {
		t.Fatalf("expected active_containers, got %q", plan.SkipReason)
	}

	targets := podmanCompactionTargets(plan)
	disk := findPodmanTarget(t, targets, "podman_vm_disk")
	if disk.Action != "protect_offline_compaction" || !disk.Protected || !disk.Active {
		t.Fatalf("expected blocked disk target for active containers: %#v", disk)
	}
	active := findPodmanTarget(t, targets, "podman_active_containers")
	if active.Action != "protect_active_containers" || !active.Protected || !active.Active {
		t.Fatalf("expected active-container target: %#v", active)
	}
}

func TestPodmanCompactionPlanRejectsMissingQemuImg(t *testing.T) {
	cfg := testPodmanCompactionConfig()

	plan := buildPodmanCompactionPlan(podmanCompactionPlanInput{
		MachineName:      "podman-machine-default",
		Provider:         "applehv",
		DiskPath:         "/Users/test/.local/share/containers/podman/machine/applehv/podman-machine-default.raw",
		ConfigEnabled:    true,
		QemuImgAvailable: false,
		DiskPathExpected: true,
		LogicalBytes:     100 * podmanCompactionGiB,
		PhysicalBytes:    12 * podmanCompactionGiB,
		FreeBytes:        20 * podmanCompactionGiB,
		Config:           cfg,
	})

	if plan.CanCompact {
		t.Fatal("expected missing qemu-img to block compaction")
	}
	if plan.SkipReason != "qemu_img_missing" {
		t.Fatalf("expected qemu_img_missing, got %q", plan.SkipReason)
	}
}

func TestPodmanCompactionTargetsExposeScratchDeficit(t *testing.T) {
	cfg := testPodmanCompactionConfig()

	plan := buildPodmanCompactionPlan(podmanCompactionPlanInput{
		MachineName:      "podman-machine-default",
		Provider:         "applehv",
		DiskPath:         "/Users/test/.local/share/containers/podman/machine/applehv/podman-machine-default.raw",
		ConfigEnabled:    true,
		QemuImgAvailable: true,
		DiskPathExpected: true,
		LogicalBytes:     100 * podmanCompactionGiB,
		PhysicalBytes:    12 * podmanCompactionGiB,
		FreeBytes:        4 * podmanCompactionGiB,
		Config:           cfg,
	})

	targets := podmanCompactionTargets(plan)
	scratch := findPodmanTarget(t, targets, "podman_compaction_scratch")
	if scratch.Action != "protect_insufficient_free_space" || !scratch.Protected {
		t.Fatalf("expected protected scratch target, got %#v", scratch)
	}
	if scratch.Bytes != plan.RequiredFreeBytes {
		t.Fatalf("scratch target should report required bytes %d, got %d", plan.RequiredFreeBytes, scratch.Bytes)
	}
	if scratch.Reclaim != CleanupReclaimNone {
		t.Fatalf("scratch target should not be counted as reclaim: %#v", scratch)
	}

	disk := findPodmanTarget(t, targets, "podman_vm_disk")
	if disk.Reclaim != CleanupReclaimNone || disk.HostReclaimsSpace == nil || *disk.HostReclaimsSpace {
		t.Fatalf("blocked disk target should not count as host reclaim: %#v", disk)
	}
}

func TestPodmanCompactionPlanUsesConfiguredScratchDir(t *testing.T) {
	cfg := testPodmanCompactionConfig()
	scratchDir := "/Volumes/TinylandSSD/tinyland-cleanup-podman"
	qemuImgPath := "/nix/store/example-qemu/bin/qemu-img"

	plan := buildPodmanCompactionPlan(podmanCompactionPlanInput{
		MachineName:          "podman-machine-default",
		Provider:             "applehv",
		DiskPath:             "/Users/test/.local/share/containers/podman/machine/applehv/podman-machine-default.raw",
		ScratchDir:           scratchDir,
		ConfigEnabled:        true,
		QemuImgPath:          qemuImgPath,
		QemuImgAvailable:     true,
		DiskPathExpected:     true,
		ScratchDirConfigured: true,
		ScratchDirAvailable:  true,
		LogicalBytes:         100 * podmanCompactionGiB,
		PhysicalBytes:        12 * podmanCompactionGiB,
		FreeBytes:            14 * podmanCompactionGiB,
		Config:               cfg,
	})

	if !plan.CanCompact {
		t.Fatalf("expected configured scratch dir compaction to be eligible, got skip reason %q", plan.SkipReason)
	}
	expectedTemp := filepath.Join(scratchDir, "podman-machine-default.raw.compact")
	if plan.TempPath != expectedTemp {
		t.Fatalf("expected temp path %q, got %q", expectedTemp, plan.TempPath)
	}
	if plan.ScratchDir != scratchDir {
		t.Fatalf("expected scratch dir %q, got %q", scratchDir, plan.ScratchDir)
	}
	if plan.QemuImgPath != qemuImgPath {
		t.Fatalf("expected qemu-img path %q, got %q", qemuImgPath, plan.QemuImgPath)
	}
	if !strings.Contains(strings.Join(plan.Steps, "\n"), qemuImgPath+" convert") {
		t.Fatalf("expected plan steps to include configured qemu-img path: %#v", plan.Steps)
	}

	targets := podmanCompactionTargets(plan)
	scratch := findPodmanTarget(t, targets, "podman_compaction_scratch")
	if scratch.Path != scratchDir {
		t.Fatalf("expected scratch target path %q, got %q", scratchDir, scratch.Path)
	}
}

func TestPodmanCompactionPlanAllowsCrossDeviceScratchDirWithBackup(t *testing.T) {
	cfg := testPodmanCompactionConfig()
	scratchDir := "/Volumes/TinylandSSD/tinyland-cleanup-podman"
	physicalBytes := int64(12 * podmanCompactionGiB)

	plan := buildPodmanCompactionPlan(podmanCompactionPlanInput{
		MachineName:           "podman-machine-default",
		Provider:              "applehv",
		DiskPath:              "/Users/test/.local/share/containers/podman/machine/applehv/podman-machine-default.raw",
		ScratchDir:            scratchDir,
		ConfigEnabled:         true,
		QemuImgAvailable:      true,
		DiskPathExpected:      true,
		ScratchDirConfigured:  true,
		ScratchDirAvailable:   true,
		ScratchDirCrossDevice: true,
		LogicalBytes:          100 * podmanCompactionGiB,
		PhysicalBytes:         physicalBytes,
		FreeBytes:             80 * podmanCompactionGiB,
		Config:                cfg,
	})

	if !plan.CanCompact {
		t.Fatalf("expected cross-device scratch dir compaction to be eligible, got skip reason %q", plan.SkipReason)
	}
	if !plan.CrossDeviceReplacement {
		t.Fatal("expected cross-device replacement mode")
	}
	expectedBackup := filepath.Join(scratchDir, "podman-machine-default.raw.backup")
	if plan.BackupPath != expectedBackup {
		t.Fatalf("expected cross-device backup path %q, got %q", expectedBackup, plan.BackupPath)
	}
	expectedRequired := podmanCompactionRequiredFreeBytes(physicalBytes, true)
	if plan.RequiredFreeBytes != expectedRequired {
		t.Fatalf("expected required free bytes %d, got %d", expectedRequired, plan.RequiredFreeBytes)
	}
	if plan.RequiredFreeBytes != podmanCompactionRequiredFreeBytes(physicalBytes, false)+physicalBytes {
		t.Fatalf("expected cross-device requirement to include rollback backup, got %d", plan.RequiredFreeBytes)
	}
	if !strings.Contains(strings.Join(plan.Steps, "\n"), "Write compacted image back") {
		t.Fatalf("expected cross-device write-back steps, got %#v", plan.Steps)
	}

	targets := podmanCompactionTargets(plan)
	scratch := findPodmanTarget(t, targets, "podman_compaction_scratch")
	if scratch.Action != "review_required_free_space" || scratch.Path != scratchDir {
		t.Fatalf("expected review scratch target, got %#v", scratch)
	}
}

func TestPodmanCompactionPlanRejectsCrossDeviceScratchDirWithoutBackup(t *testing.T) {
	cfg := testPodmanCompactionConfig()
	cfg.CompactKeepBackupUntilRestart = false
	scratchDir := "/Volumes/TinylandSSD/tinyland-cleanup-podman"

	plan := buildPodmanCompactionPlan(podmanCompactionPlanInput{
		MachineName:           "podman-machine-default",
		Provider:              "applehv",
		DiskPath:              "/Users/test/.local/share/containers/podman/machine/applehv/podman-machine-default.raw",
		ScratchDir:            scratchDir,
		ConfigEnabled:         true,
		QemuImgAvailable:      true,
		DiskPathExpected:      true,
		ScratchDirConfigured:  true,
		ScratchDirAvailable:   true,
		ScratchDirCrossDevice: true,
		LogicalBytes:          100 * podmanCompactionGiB,
		PhysicalBytes:         12 * podmanCompactionGiB,
		FreeBytes:             80 * podmanCompactionGiB,
		Config:                cfg,
	})

	if plan.CanCompact {
		t.Fatal("expected cross-device scratch dir without rollback backup to block compaction")
	}
	if plan.SkipReason != "cross_device_backup_disabled" {
		t.Fatalf("expected cross_device_backup_disabled, got %q", plan.SkipReason)
	}

	targets := podmanCompactionTargets(plan)
	scratch := findPodmanTarget(t, targets, "podman_compaction_scratch")
	if scratch.Action != "protect_cross_device_backup_disabled" || scratch.Path != scratchDir {
		t.Fatalf("expected protected cross-device backup target, got %#v", scratch)
	}
}

func TestPodmanCompactionPlanRejectsUnavailableScratchDir(t *testing.T) {
	cfg := testPodmanCompactionConfig()
	scratchDir := "/Volumes/TinylandSSD/tinyland-cleanup-podman"

	plan := buildPodmanCompactionPlan(podmanCompactionPlanInput{
		MachineName:          "podman-machine-default",
		Provider:             "applehv",
		DiskPath:             "/Users/test/.local/share/containers/podman/machine/applehv/podman-machine-default.raw",
		ScratchDir:           scratchDir,
		ConfigEnabled:        true,
		QemuImgAvailable:     true,
		DiskPathExpected:     true,
		ScratchDirConfigured: true,
		ScratchDirAvailable:  false,
		LogicalBytes:         100 * podmanCompactionGiB,
		PhysicalBytes:        12 * podmanCompactionGiB,
		FreeBytes:            0,
		Config:               cfg,
	})

	if plan.CanCompact {
		t.Fatal("expected unavailable scratch dir to block compaction")
	}
	if plan.SkipReason != "scratch_dir_unavailable" {
		t.Fatalf("expected scratch_dir_unavailable, got %q", plan.SkipReason)
	}

	targets := podmanCompactionTargets(plan)
	scratch := findPodmanTarget(t, targets, "podman_compaction_scratch")
	if scratch.Action != "protect_scratch_dir_unavailable" || scratch.Path != scratchDir {
		t.Fatalf("expected protected unavailable scratch target, got %#v", scratch)
	}
}

func TestPodmanCompactionTargetsEligibleDiskReclaimsHost(t *testing.T) {
	cfg := testPodmanCompactionConfig()

	plan := buildPodmanCompactionPlan(podmanCompactionPlanInput{
		MachineName:      "podman-machine-default",
		Provider:         "applehv",
		DiskPath:         "/Users/test/.local/share/containers/podman/machine/applehv/podman-machine-default.raw",
		ConfigEnabled:    true,
		QemuImgAvailable: true,
		DiskPathExpected: true,
		LogicalBytes:     100 * podmanCompactionGiB,
		PhysicalBytes:    12 * podmanCompactionGiB,
		FreeBytes:        14 * podmanCompactionGiB,
		Config:           cfg,
	})

	targets := podmanCompactionTargets(plan)
	disk := findPodmanTarget(t, targets, "podman_vm_disk")
	if disk.Action != "compact_disk_offline" || disk.Protected {
		t.Fatalf("expected eligible disk compaction target: %#v", disk)
	}
	if disk.Reclaim != CleanupReclaimHost || disk.HostReclaimsSpace == nil || !*disk.HostReclaimsSpace {
		t.Fatalf("eligible disk target should count as host reclaim: %#v", disk)
	}
	if disk.Bytes != plan.EstimatedReclaimBytes {
		t.Fatalf("disk target should carry estimated reclaim %d, got %d", plan.EstimatedReclaimBytes, disk.Bytes)
	}
}

func TestPathWithinRoots(t *testing.T) {
	root := t.TempDir()
	diskPath := filepath.Join(root, "applehv", "podman-machine-default.raw")

	if !pathWithinRoots(diskPath, []string{root}) {
		t.Fatalf("expected %s to be within %s", diskPath, root)
	}
	if pathWithinRoots(filepath.Join(t.TempDir(), "outside.raw"), []string{root}) {
		t.Fatal("expected unrelated path to be outside root")
	}
}

func findPodmanTarget(t *testing.T, targets []CleanupTarget, targetType string) CleanupTarget {
	t.Helper()
	for _, target := range targets {
		if target.Type == targetType {
			return target
		}
	}
	t.Fatalf("target %s not found in %#v", targetType, targets)
	return CleanupTarget{}
}

func testPodmanCompactionConfig() config.PodmanConfig {
	cfg := config.DefaultConfig().Podman
	cfg.CompactDiskOffline = true
	cfg.CompactMinReclaimGB = 8
	cfg.CompactRequireNoActiveContainers = true
	cfg.CompactKeepBackupUntilRestart = true
	cfg.CompactProviderAllowlist = []string{"applehv", "libkrun", "qemu"}
	return cfg
}
