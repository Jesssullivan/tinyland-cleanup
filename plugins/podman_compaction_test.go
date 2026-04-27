package plugins

import (
	"path/filepath"
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
