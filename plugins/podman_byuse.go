package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

// Podman prune by use (TIN-8526, ruling R-STING-HOME-20261117-c). On Linux
// rootless podman the pass removes each image that no container uses and
// each dangling volume, once it is older than podman.prune_images_age. It is
// off unless podman.prune_by_use is "report" or "apply".
//
// Each removal is planned from podman's own listings, re-checked against a
// fresh `podman image inspect` or `podman volume inspect` and the keep file
// just before the act, and then made with `podman rmi <id>` or
// `podman volume rm <name>` without --force. Podman takes its own lock for
// the removal and refuses an image or volume that any container uses at that
// moment, in any state.
//
// Podman records no last use. An image's age is taken from the later of its
// build time (`Created`) and the time it entered the local store (the
// modification or change time of its directory under
// <graphroot>/<driver>-images/<id>). Podman writes that directory when it
// stores the image, so a pulled or loaded image is kept for
// prune_images_age after that write, however old its build. An image that a
// container used, where that container was then removed, is eligible at once
// if it is otherwise old enough: nothing records that use.
const (
	podmanByUseOff    = "off"
	podmanByUseReport = "report"
	podmanByUseApply  = "apply"
)

// podmanByUseMinAge is the shortest prune_images_age the pass accepts. It
// leaves a just-stored image at least an hour to gain its first container.
const podmanByUseMinAge = time.Hour

// podmanByUseGOOS is runtime.GOOS; tests set it to prove the Darwin paths
// never reach the pass.
var podmanByUseGOOS = runtime.GOOS

var (
	podmanFullIDPattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	podmanVolumeNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	podmanDriverPattern     = regexp.MustCompile(`^[a-z0-9_]+$`)
)

// podmanByUseMode returns the configured mode. Any value other than "report"
// or "apply" is off; config validation refuses such values at load.
func podmanByUseMode(cfg *config.Config) string {
	switch strings.ToLower(strings.TrimSpace(cfg.Podman.PruneByUse)) {
	case podmanByUseReport:
		return podmanByUseReport
	case podmanByUseApply:
		return podmanByUseApply
	default:
		return podmanByUseOff
	}
}

// byUseOn reports whether the pass runs on this host in mode report or apply.
func (p *PodmanPlugin) byUseOn(cfg *config.Config) bool {
	return podmanByUseMode(cfg) != podmanByUseOff && p.byUsePlatform()
}

// byUseApplies reports whether the pass may remove on this host.
func (p *PodmanPlugin) byUseApplies(cfg *config.Config) bool {
	return podmanByUseMode(cfg) == podmanByUseApply && p.byUsePlatform()
}

func (p *PodmanPlugin) byUsePlatform() bool {
	return podmanByUseGOOS == "linux" && p.environment != nil && !p.environment.NeedsVM
}

// podmanByUseRunsAt reports whether Cleanup reaches the pass at level. The
// moderate pass runs at moderate and aggressive. At critical it runs when the
// broad system prune does not: the system prune is off, or the keep rule
// sends critical through the aggressive passes.
func podmanByUseRunsAt(level CleanupLevel, cfg *config.Config, keep podmanKeep) bool {
	switch level {
	case LevelModerate, LevelAggressive:
		return true
	case LevelCritical:
		return !cfg.Podman.CriticalSystemPrune || keep.active()
	default:
		return false
	}
}

// podmanByUseImage is the part of `podman images --format json` the pass
// reads. Containers is a pointer so a listing without the field is kept.
type podmanByUseImage struct {
	ID          string            `json:"Id"`
	Names       []string          `json:"Names"`
	RepoDigests []string          `json:"RepoDigests"`
	Containers  *int              `json:"Containers"`
	ReadOnly    bool              `json:"ReadOnly"`
	Created     int64             `json:"Created"`
	Size        int64             `json:"Size"`
	Labels      map[string]string `json:"Labels"`
}

// podmanByUseVolume is the part of `podman volume ls --format json` and
// `podman volume inspect --format json` the pass reads.
type podmanByUseVolume struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Mountpoint string            `json:"Mountpoint"`
	CreatedAt  string            `json:"CreatedAt"`
	Labels     map[string]string `json:"Labels"`
}

// podmanStoreInfo is the answer to one closed `podman info` question.
type podmanStoreInfo struct {
	Rootless  bool
	GraphRoot string
	Driver    string
}

// podmanByUseItem is one image or volume the pass considered.
type podmanByUseItem struct {
	Kind    string // "image" or "volume"
	ID      string // full image ID or volume name
	Label   string // first image name, or the volume name
	Bytes   int64
	Created time.Time // volume CreatedAt, compared again before removal
	Remove  bool
	Reason  string
}

type podmanByUsePlan struct {
	Mode       string
	Age        time.Duration
	SkipReason string
	Warnings   []string
	Store      podmanStoreInfo
	Keep       podmanKeep
	Protected  map[string]string
	Items      []podmanByUseItem
}

func (pl podmanByUsePlan) counts() (imagesRemove, imagesKeep, volumesRemove, volumesKeep int, bytes int64) {
	for _, it := range pl.Items {
		switch {
		case it.Kind == "image" && it.Remove:
			imagesRemove++
			bytes += it.Bytes
		case it.Kind == "image":
			imagesKeep++
		case it.Remove:
			volumesRemove++
			bytes += it.Bytes
		default:
			volumesKeep++
		}
	}
	return
}

// podmanImageStoreTime returns the later of the modification and change time
// of the image's directory in the local store. Anything but a directory is an
// error, so the image is kept. Tests replace it.
var podmanImageStoreTime = func(store podmanStoreInfo, id string) (time.Time, error) {
	dir := filepath.Join(store.GraphRoot, store.Driver+"-images", id)
	info, err := os.Lstat(dir)
	if err != nil {
		return time.Time{}, err
	}
	if !info.IsDir() {
		return time.Time{}, fmt.Errorf("%s is not a directory (%s)", dir, info.Mode().Type())
	}
	t := info.ModTime()
	if c, ok := changeTime(info); ok && c.After(t) {
		t = c
	}
	return t, nil
}

// podmanVolumeBytes measures a removal candidate's data. Files the daemon
// cannot read are not counted, so the figure is a lower bound. Tests replace
// it.
var podmanVolumeBytes = func(ctx context.Context, store podmanStoreInfo, v podmanByUseVolume) int64 {
	want := filepath.Join(store.GraphRoot, "volumes", v.Name, "_data")
	if v.Mountpoint != want {
		return 0
	}
	size, _ := getDirAllocatedBytesContext(ctx, want)
	return size
}

const podmanByUseInfoFormat = "{{.Host.Security.Rootless}}\t{{.Store.GraphRoot}}\t{{.Store.GraphDriverName}}"

// parsePodmanStoreInfo accepts exactly "<true|false>\t<absolute clean
// path>\t<driver>" and rejects everything else.
func parsePodmanStoreInfo(output string) (podmanStoreInfo, error) {
	fields := strings.Split(strings.TrimSpace(output), "\t")
	if len(fields) != 3 {
		return podmanStoreInfo{}, fmt.Errorf("unexpected podman info output %q", output)
	}
	var info podmanStoreInfo
	switch fields[0] {
	case "true":
		info.Rootless = true
	case "false":
	default:
		return podmanStoreInfo{}, fmt.Errorf("unexpected rootless value %q", fields[0])
	}
	info.GraphRoot = fields[1]
	if !filepath.IsAbs(info.GraphRoot) || filepath.Clean(info.GraphRoot) != info.GraphRoot || info.GraphRoot == "/" {
		return podmanStoreInfo{}, fmt.Errorf("unexpected graph root %q", info.GraphRoot)
	}
	info.Driver = fields[2]
	if !podmanDriverPattern.MatchString(info.Driver) {
		return podmanStoreInfo{}, fmt.Errorf("unexpected graph driver %q", info.Driver)
	}
	return info, nil
}

func parsePodmanByUseJSON[T any](output string) ([]T, error) {
	output = strings.TrimSpace(output)
	if output == "" || output == "null" {
		return nil, nil
	}
	var out []T
	if err := json.Unmarshal([]byte(output), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// podmanKeepRefSet holds the configured references, so an image listed under
// one of them is kept even before `podman image inspect` resolves it.
func podmanKeepRefSet(keep podmanKeep) map[string]struct{} {
	set := make(map[string]struct{}, len(keep.Refs))
	for _, ref := range keep.Refs {
		set[ref] = struct{}{}
	}
	return set
}

func podmanNamesPinned(set map[string]struct{}, names ...[]string) bool {
	for _, list := range names {
		for _, n := range list {
			if _, ok := set[n]; ok {
				return true
			}
		}
	}
	return false
}

// selectPodmanByUseImages decides each listed image from the listing, the
// resolved pins and the store time. Unknown means keep.
func selectPodmanByUseImages(images []podmanByUseImage, protected map[string]string, keep podmanKeep, store podmanStoreInfo, age time.Duration, now time.Time) []podmanByUseItem {
	refs := podmanKeepRefSet(keep)
	cutoff := now.Add(-age)
	seen := make(map[string]struct{}, len(images))
	var out []podmanByUseItem
	for _, img := range images {
		if _, dup := seen[img.ID]; dup {
			continue
		}
		seen[img.ID] = struct{}{}
		item := podmanByUseItem{Kind: "image", ID: img.ID, Label: img.ID, Bytes: img.Size}
		if len(img.Names) > 0 {
			item.Label = img.Names[0]
		}
		item.Reason = podmanByUseImageKeepReason(img, protected, refs, keep, store, cutoff)
		if item.Reason == "" {
			item.Remove = true
			item.Reason = fmt.Sprintf("no container uses it and it is older than %s", age)
		}
		out = append(out, item)
	}
	return out
}

func podmanByUseImageKeepReason(img podmanByUseImage, protected map[string]string, refs map[string]struct{}, keep podmanKeep, store podmanStoreInfo, cutoff time.Time) string {
	if !podmanFullIDPattern.MatchString(img.ID) {
		return "image ID is not a full podman ID"
	}
	if ref, ok := protected[img.ID]; ok {
		return "pinned by " + ref
	}
	if podmanNamesPinned(refs, img.Names, img.RepoDigests) {
		return "a name or digest is pinned"
	}
	if keep.protectsLabels(img.Labels) {
		return "carries a protected label"
	}
	if img.Containers == nil {
		return "container count missing from the listing"
	}
	if *img.Containers != 0 {
		return fmt.Sprintf("used by %d container(s)", *img.Containers)
	}
	if img.ReadOnly {
		return "read-only image from an additional store"
	}
	if img.Created <= 0 {
		return "creation time unknown"
	}
	stored, err := podmanImageStoreTime(store, img.ID)
	if err != nil {
		return "store time unknown: " + err.Error()
	}
	newest := time.Unix(img.Created, 0)
	if stored.After(newest) {
		newest = stored
	}
	if !newest.Before(cutoff) {
		return "built or stored within prune_images_age"
	}
	return ""
}

// selectPodmanByUseVolumes decides each dangling volume. The caller lists
// only `--filter dangling=true` volumes.
func selectPodmanByUseVolumes(volumes []podmanByUseVolume, keep podmanKeep, age time.Duration, now time.Time) []podmanByUseItem {
	cutoff := now.Add(-age)
	seen := make(map[string]struct{}, len(volumes))
	var out []podmanByUseItem
	for _, v := range volumes {
		if _, dup := seen[v.Name]; dup {
			continue
		}
		seen[v.Name] = struct{}{}
		item := podmanByUseItem{Kind: "volume", ID: v.Name, Label: v.Name}
		created, reason := podmanByUseVolumeKeepReason(v, keep, cutoff)
		item.Created = created
		item.Reason = reason
		if reason == "" {
			item.Remove = true
			item.Reason = fmt.Sprintf("no container uses it and it is older than %s", age)
		}
		out = append(out, item)
	}
	return out
}

func podmanByUseVolumeKeepReason(v podmanByUseVolume, keep podmanKeep, cutoff time.Time) (time.Time, string) {
	if !podmanVolumeNamePattern.MatchString(v.Name) {
		return time.Time{}, "volume name is not a plain podman name"
	}
	if v.Driver != "local" {
		return time.Time{}, "volume driver is not local"
	}
	if keep.protectsLabels(v.Labels) {
		return time.Time{}, "carries a protected label"
	}
	created, err := time.Parse(time.RFC3339Nano, v.CreatedAt)
	if err != nil || created.IsZero() {
		return time.Time{}, "creation time unknown"
	}
	if !created.Before(cutoff) {
		return created, "created within prune_images_age"
	}
	return created, ""
}

// planByUse builds the pass's plan from podman's listings. It removes
// nothing.
func (p *PodmanPlugin) planByUse(ctx context.Context, cfg *config.Config, now time.Time, logger *slog.Logger) podmanByUsePlan {
	plan := podmanByUsePlan{Mode: podmanByUseMode(cfg)}
	if plan.Mode == podmanByUseOff {
		plan.SkipReason = "prune_by_use_off"
		return plan
	}
	if !p.byUsePlatform() {
		plan.SkipReason = "prune_by_use_linux_only"
		return plan
	}
	age, err := config.ParseDuration(cfg.Podman.PruneImagesAge)
	if err != nil {
		plan.SkipReason = "prune_images_age_invalid"
		plan.Warnings = append(plan.Warnings, err.Error())
		return plan
	}
	if age < podmanByUseMinAge {
		plan.SkipReason = "prune_images_age_below_floor"
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("prune_images_age %s is below the %s floor of the prune-by-use pass", age, podmanByUseMinAge))
		return plan
	}
	plan.Age = age

	plan.Keep = loadPodmanKeep(cfg.Podman, podmanHome())
	if plan.Keep.FileErr != nil {
		plan.SkipReason = "protected_images_file_unreadable"
		plan.Warnings = append(plan.Warnings, plan.Keep.FileErr.Error())
		return plan
	}

	out, err := p.runPodmanCommand(ctx, "info", "--format", podmanByUseInfoFormat)
	if err != nil {
		plan.SkipReason = "podman_store_unknown"
		plan.Warnings = append(plan.Warnings, "podman info failed: "+err.Error())
		return plan
	}
	plan.Store, err = parsePodmanStoreInfo(out)
	if err != nil {
		plan.SkipReason = "podman_store_unknown"
		plan.Warnings = append(plan.Warnings, err.Error())
		return plan
	}
	if !plan.Store.Rootless {
		plan.SkipReason = "podman_not_rootless"
		return plan
	}

	protected, ok := p.resolveProtectedImageIDs(ctx, plan.Keep.Refs, logger)
	if !ok {
		plan.SkipReason = "protected_image_unresolved"
		return plan
	}
	plan.Protected = protected

	out, err = p.runPodmanCommand(ctx, "images", "--format", "json")
	if err != nil {
		plan.SkipReason = "image_listing_failed"
		plan.Warnings = append(plan.Warnings, "podman images failed: "+err.Error())
		return plan
	}
	images, err := parsePodmanByUseJSON[podmanByUseImage](out)
	if err != nil {
		plan.SkipReason = "image_listing_failed"
		plan.Warnings = append(plan.Warnings, "podman images output: "+err.Error())
		return plan
	}
	out, err = p.runPodmanCommand(ctx, "volume", "ls", "--format", "json", "--filter", "dangling=true")
	if err != nil {
		plan.SkipReason = "volume_listing_failed"
		plan.Warnings = append(plan.Warnings, "podman volume ls failed: "+err.Error())
		return plan
	}
	volumes, err := parsePodmanByUseJSON[podmanByUseVolume](out)
	if err != nil {
		plan.SkipReason = "volume_listing_failed"
		plan.Warnings = append(plan.Warnings, "podman volume ls output: "+err.Error())
		return plan
	}

	plan.Items = selectPodmanByUseImages(images, protected, plan.Keep, plan.Store, age, now)
	byName := make(map[string]podmanByUseVolume, len(volumes))
	for _, v := range volumes {
		byName[v.Name] = v
	}
	for _, item := range selectPodmanByUseVolumes(volumes, plan.Keep, age, now) {
		if item.Remove {
			item.Bytes = podmanVolumeBytes(ctx, plan.Store, byName[item.ID])
		}
		plan.Items = append(plan.Items, item)
	}
	return plan
}

// recheckPodmanByUseItem asks podman about one item again just before its
// removal and returns "" only when the fresh answer still allows it. The
// keep file is read after podman answers, as the last step before removal.
func (p *PodmanPlugin) recheckPodmanByUseItem(ctx context.Context, cfg *config.Config, plan podmanByUsePlan, item podmanByUseItem, now time.Time) string {
	cutoff := now.Add(-plan.Age)
	keepNow := func() (podmanKeep, bool) {
		keep := loadPodmanKeep(cfg.Podman, podmanHome())
		same := keep.FileErr == nil && strings.Join(keep.Refs, "\n") == strings.Join(plan.Keep.Refs, "\n")
		return keep, same
	}
	const keepChanged = "keep file changed or became unreadable during the pass"
	if item.Kind == "image" {
		out, err := p.runPodmanCommand(ctx, "image", "inspect", "--format", "json", item.ID)
		if err != nil {
			return "image inspect failed"
		}
		keep, same := keepNow()
		if !same {
			return keepChanged
		}
		var got []struct {
			ID          string            `json:"Id"`
			Created     string            `json:"Created"`
			Labels      map[string]string `json:"Labels"`
			RepoTags    []string          `json:"RepoTags"`
			RepoDigests []string          `json:"RepoDigests"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil || len(got) != 1 || got[0].ID != item.ID {
			return "image inspect answer unexpected"
		}
		created, err := time.Parse(time.RFC3339Nano, got[0].Created)
		if err != nil || created.IsZero() {
			return "image creation time unknown"
		}
		zero := 0
		img := podmanByUseImage{
			ID:          got[0].ID,
			Names:       got[0].RepoTags,
			RepoDigests: got[0].RepoDigests,
			Containers:  &zero, // podman rmi without --force refuses an image in use
			Created:     created.Unix(),
			Labels:      got[0].Labels,
		}
		return podmanByUseImageKeepReason(img, plan.Protected, podmanKeepRefSet(keep), keep, plan.Store, cutoff)
	}
	out, err := p.runPodmanCommand(ctx, "volume", "inspect", "--format", "json", item.ID)
	if err != nil {
		return "volume inspect failed"
	}
	got, err := parsePodmanByUseJSON[podmanByUseVolume](out)
	if err != nil || len(got) != 1 || got[0].Name != item.ID {
		return "volume inspect answer unexpected"
	}
	keep, same := keepNow()
	if !same {
		return keepChanged
	}
	created, reason := podmanByUseVolumeKeepReason(got[0], keep, cutoff)
	if reason != "" {
		return reason
	}
	if !created.Equal(item.Created) {
		return "volume was recreated since the listing"
	}
	return ""
}

// pruneByUse runs the pass. In report mode it logs the plan and removes
// nothing. In apply mode it re-checks and removes each planned item and
// returns the listed sizes of the removals podman accepted.
func (p *PodmanPlugin) pruneByUse(ctx context.Context, cfg *config.Config, logger *slog.Logger) (int64, int) {
	plan := p.planByUse(ctx, cfg, time.Now(), logger)
	if plan.SkipReason != "" {
		logger.Warn("skipping podman prune-by-use", "reason", plan.SkipReason, "warnings", strings.Join(plan.Warnings, "; "))
		return 0, 0
	}
	imagesRemove, imagesKeep, volumesRemove, volumesKeep, planned := plan.counts()
	logger.Info("podman prune-by-use plan",
		"mode", plan.Mode, "age", plan.Age.String(),
		"images_remove", imagesRemove, "images_keep", imagesKeep,
		"volumes_remove", volumesRemove, "volumes_keep", volumesKeep,
		"planned_bytes", planned)
	if plan.Mode != podmanByUseApply {
		for _, it := range plan.Items {
			if it.Remove {
				logger.Debug("podman prune-by-use would remove", "kind", it.Kind, "id", it.ID, "name", it.Label, "bytes", it.Bytes)
			}
		}
		return 0, 0
	}

	var freed int64
	removed, refused := 0, 0
	for _, it := range plan.Items {
		if !it.Remove {
			continue
		}
		if ctx.Err() != nil {
			logger.Warn("podman prune-by-use stopped", "reason", ctx.Err())
			break
		}
		if reason := p.recheckPodmanByUseItem(ctx, cfg, plan, it, time.Now()); reason != "" {
			logger.Info("podman prune-by-use kept after re-check", "kind", it.Kind, "id", it.ID, "name", it.Label, "reason", reason)
			refused++
			continue
		}
		args := []string{"rmi", it.ID}
		if it.Kind == "volume" {
			args = []string{"volume", "rm", it.ID}
		}
		if out, err := p.runPodmanCommand(ctx, args...); err != nil {
			logger.Info("podman refused prune-by-use removal", "kind", it.Kind, "id", it.ID, "name", it.Label, "error", err, "output", strings.TrimSpace(out))
			refused++
			continue
		}
		freed += it.Bytes
		removed++
		logger.Info("podman prune-by-use removed", "kind", it.Kind, "id", it.ID, "name", it.Label, "bytes", it.Bytes)
	}
	logger.Info("podman prune-by-use done", "removed", removed, "refused_or_kept", refused, "listed_bytes", freed)
	return freed, removed
}

// addPodmanByUsePlanToCleanupPlan reports the pass in a dry-run plan.
func addPodmanByUsePlanToCleanupPlan(plan *CleanupPlan, byUse podmanByUsePlan) {
	plan.Metadata["prune_by_use_mode"] = byUse.Mode
	plan.Metadata["prune_by_use_skip_reason"] = byUse.SkipReason
	if byUse.SkipReason != "" {
		plan.Steps = append(plan.Steps, "Skip the Podman prune-by-use pass: "+byUse.SkipReason)
		plan.Warnings = append(plan.Warnings, byUse.Warnings...)
		return
	}
	imagesRemove, imagesKeep, volumesRemove, volumesKeep, planned := byUse.counts()
	plan.Metadata["prune_by_use_images_remove"] = strconv.Itoa(imagesRemove)
	plan.Metadata["prune_by_use_images_keep"] = strconv.Itoa(imagesKeep)
	plan.Metadata["prune_by_use_volumes_remove"] = strconv.Itoa(volumesRemove)
	plan.Metadata["prune_by_use_volumes_keep"] = strconv.Itoa(volumesKeep)
	plan.Metadata["prune_by_use_planned_bytes"] = strconv.FormatInt(planned, 10)
	verb := "Report"
	if byUse.Mode == podmanByUseApply {
		verb = "Remove"
		plan.EstimatedBytesFreed += planned
	}
	plan.Steps = append(plan.Steps, fmt.Sprintf("%s %d unused Podman image(s) and %d dangling volume(s) older than %s, re-checked before each removal", verb, imagesRemove, volumesRemove, byUse.Age))
	plan.Warnings = append(plan.Warnings, "prune-by-use bytes are listed image sizes, which count shared layers once per image, plus readable volume data; the measured host free-space delta is authoritative")
	for _, it := range byUse.Items {
		target := CleanupTarget{
			Type:   "podman_" + it.Kind,
			Name:   it.Label,
			Bytes:  it.Bytes,
			Reason: it.Reason,
		}
		switch {
		case it.Remove && byUse.Mode == podmanByUseApply:
			target.Action = "delete_podman_" + it.Kind
			annotateCleanupTargetPolicy(&target, CleanupTierWarm, CleanupReclaimHost)
		case it.Remove:
			target.Action = "report"
			target.Reason = "report mode: " + it.Reason
			annotateCleanupTargetPolicy(&target, CleanupTierWarm, CleanupReclaimNone)
		default:
			target.Action = "keep"
			target.Protected = true
			annotateCleanupTargetPolicy(&target, CleanupTierWarm, CleanupReclaimNone)
		}
		plan.Targets = append(plan.Targets, target)
	}
}
