// scan_budget.go holds the dev-artifacts scan budget: the entry and duration
// bounds that keep filesystem walks finite, and the per-root shares that keep
// one large root from ending the whole pass (TIN-3342 PR4, issue #129).
package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

var errDevArtifactScanBudgetExceeded = errors.New("dev artifact scan budget exceeded")

// maxRecordedTruncations bounds each truncation map so a pathological scan
// cannot grow report metadata without limit.
const maxRecordedTruncations = 20

type devArtifactScanBudget struct {
	maxDuration             time.Duration
	maxEntries              int
	workspaceMaxRoots       int
	workspaceCursorFile     string
	workspaceCursorState    devArtifactWorkspaceCursorState
	persistWorkspaceCursors bool
	tempMaxRoots            int

	entries               int
	sizingEntries         int
	workspaceRootsVisited int
	workspaceRootsSkipped int
	tempRoots             int
	tempRootSeen          map[string]struct{}
	// truncatedPath records truncations of this budget itself. On the shared
	// budget they gate the remaining walk lanes (see exhausted).
	truncatedPath map[string]string
	// rootTruncatedPath records truncations inside one root's own share: a
	// single temp scan path or a single workspace root. They are reported as
	// partial evidence, and they never gate the remaining roots or lanes:
	// one huge root must not starve the others (OI-1001-Q4 for temp paths,
	// TIN-3342 PR4 for workspace roots). Entries still fold back into the
	// shared count, so the post-workspace lanes that walk against the shared
	// budget stop when it runs out. Artifact-family walks are bounded per
	// family by their root pool; their callers pass the outer context, so
	// the shared deadline does not bound them.
	rootTruncatedPath       map[string]string
	tempPathsTruncated      int
	workspaceRootsTruncated int
}

type devArtifactWorkspaceCursorState struct {
	Version int               `json:"version"`
	Cursors map[string]string `json:"cursors"`
}

func newDevArtifactScanBudget(cfg config.DevArtifactsConfig) *devArtifactScanBudget {
	return &devArtifactScanBudget{
		maxDuration:       parseNixPolicyDuration(cfg.ScanMaxDuration, 30*time.Second),
		maxEntries:        cfg.ScanMaxEntries,
		workspaceMaxRoots: cfg.WorkspaceScanMaxRoots,
		tempMaxRoots:      cfg.TempScanMaxRoots,
		tempRootSeen:      map[string]struct{}{},
		truncatedPath:     map[string]string{},
		rootTruncatedPath: map[string]string{},
	}
}

func newDevArtifactScanBudgetForConfig(cfg *config.Config, persistWorkspaceCursors bool) *devArtifactScanBudget {
	budget := newDevArtifactScanBudget(cfg.DevArtifacts)
	budget.workspaceCursorFile = devArtifactWorkspaceCursorFile(cfg.Policy.StateFile)
	budget.persistWorkspaceCursors = persistWorkspaceCursors
	budget.loadWorkspaceCursors()
	return budget
}

func devArtifactWorkspaceCursorFile(stateFile string) string {
	if stateFile == "" {
		return ""
	}
	home, _ := os.UserHomeDir()
	expanded := expandHome(stateFile, home)
	ext := filepath.Ext(expanded)
	if ext == "" {
		return expanded + ".dev-artifacts.json"
	}
	return strings.TrimSuffix(expanded, ext) + ".dev-artifacts" + ext
}

func (b *devArtifactScanBudget) loadWorkspaceCursors() {
	if b == nil || b.workspaceCursorFile == "" {
		return
	}
	data, err := os.ReadFile(b.workspaceCursorFile)
	if err != nil {
		return
	}
	var state devArtifactWorkspaceCursorState
	if err := json.Unmarshal(data, &state); err != nil {
		return
	}
	if state.Cursors == nil {
		state.Cursors = map[string]string{}
	}
	b.workspaceCursorState = state
}

func (b *devArtifactScanBudget) saveWorkspaceCursors() {
	if b == nil || !b.persistWorkspaceCursors || b.workspaceCursorFile == "" || len(b.workspaceCursorState.Cursors) == 0 {
		return
	}
	state := b.workspaceCursorState
	state.Version = 1
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(b.workspaceCursorFile), 0755); err != nil {
		return
	}
	tmp := b.workspaceCursorFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return
	}
	_ = os.Rename(tmp, b.workspaceCursorFile)
}

func (b *devArtifactScanBudget) selectWorkspaceRoots(key string, roots []string) []string {
	if b == nil || len(roots) == 0 {
		return roots
	}
	selected := roots
	if b.workspaceMaxRoots > 0 && len(roots) > b.workspaceMaxRoots {
		selected = rotateWorkspaceRootsAfterCursor(roots, b.workspaceCursorState.Cursors[key], b.workspaceMaxRoots)
		b.workspaceRootsSkipped += len(roots) - len(selected)
	}
	b.workspaceRootsVisited += len(selected)
	return selected
}

func rotateWorkspaceRootsAfterCursor(roots []string, cursor string, maxRoots int) []string {
	if maxRoots <= 0 || len(roots) <= maxRoots {
		return roots
	}
	start := 0
	if cursor != "" {
		start = len(roots)
		for idx, root := range roots {
			if root > cursor {
				start = idx
				break
			}
		}
		if start >= len(roots) {
			start = 0
		}
	}
	selected := make([]string, 0, maxRoots)
	for offset := 0; offset < len(roots) && len(selected) < maxRoots; offset++ {
		selected = append(selected, roots[(start+offset)%len(roots)])
	}
	return selected
}

func (b *devArtifactScanBudget) recordWorkspaceCursor(key string, root string) {
	if b == nil || !b.persistWorkspaceCursors || b.workspaceCursorFile == "" || key == "" || root == "" {
		return
	}
	if b.workspaceCursorState.Cursors == nil {
		b.workspaceCursorState.Cursors = map[string]string{}
	}
	b.workspaceCursorState.Cursors[key] = filepath.Clean(root)
	b.saveWorkspaceCursors()
}

// devArtifactRootPool hands out per-root budgets for one lane that walks a
// list of roots (the selected workspace roots of one artifact family, or the
// temp scan paths). The pool is the lane's configured entry and duration
// budget. Each root's share is what the pool has left divided by the roots
// still to come, so a root that uses less than its share leaves the rest to
// the roots after it (carry-over). A root that uses its whole share is
// truncated on its own and the lane moves on to the next root.
type devArtifactRootPool struct {
	maxEntries     int
	maxDuration    time.Duration
	usedEntries    int
	started        time.Time
	remainingRoots int
	now            func() time.Time
}

func (b *devArtifactScanBudget) rootPool(roots int) *devArtifactRootPool {
	if b == nil {
		return nil
	}
	return &devArtifactRootPool{
		maxEntries:     b.maxEntries,
		maxDuration:    b.maxDuration,
		started:        time.Now(),
		remainingRoots: roots,
		now:            time.Now,
	}
}

// entryShare returns the next root's entry share without consuming it.
// Zero means unbounded.
func (pl *devArtifactRootPool) entryShare() int {
	if pl == nil || pl.maxEntries <= 0 {
		return 0
	}
	roots := pl.remainingRoots
	if roots < 1 {
		roots = 1
	}
	left := pl.maxEntries - pl.usedEntries
	share := (left + roots - 1) / roots
	if share < 1 {
		share = 1
	}
	return share
}

// durationShare returns the next root's duration share without consuming it.
// Zero means unbounded.
func (pl *devArtifactRootPool) durationShare() time.Duration {
	if pl == nil || pl.maxDuration <= 0 {
		return 0
	}
	roots := pl.remainingRoots
	if roots < 1 {
		roots = 1
	}
	left := pl.maxDuration - pl.now().Sub(pl.started)
	share := left / time.Duration(roots)
	if share < time.Second {
		share = time.Second
	}
	return share
}

// child returns the next root's budget.
func (pl *devArtifactRootPool) child(tempMaxRoots int) *devArtifactScanBudget {
	if pl == nil {
		return nil
	}
	child := &devArtifactScanBudget{
		maxDuration:       pl.durationShare(),
		maxEntries:        pl.entryShare(),
		tempMaxRoots:      tempMaxRoots,
		tempRootSeen:      map[string]struct{}{},
		truncatedPath:     map[string]string{},
		rootTruncatedPath: map[string]string{},
	}
	if pl.remainingRoots > 0 {
		pl.remainingRoots--
	}
	return child
}

// done returns a finished root's unused share to the pool.
func (pl *devArtifactRootPool) done(child *devArtifactScanBudget) {
	if pl == nil || child == nil {
		return
	}
	pl.usedEntries += child.entries
}

func (b *devArtifactScanBudget) mergeRootBudget(child *devArtifactScanBudget) {
	b.entries += child.entries
	b.sizingEntries += child.sizingEntries
	for path, reason := range child.truncatedPath {
		b.markRootTruncated(path, reason)
	}
	for path, reason := range child.rootTruncatedPath {
		b.markRootTruncated(path, reason)
	}
}

// mergeWorkspaceRootBudget folds a finished workspace root's accounting back
// into the shared budget. Its truncations are partial evidence only; they do
// not exhaust the shared budget.
func (b *devArtifactScanBudget) mergeWorkspaceRootBudget(child *devArtifactScanBudget) {
	if b == nil || child == nil || child == b {
		return
	}
	if child.exhausted() {
		b.workspaceRootsTruncated++
	}
	b.mergeRootBudget(child)
}

// mergeTempPathBudget folds a finished temp scan path's accounting back into
// the shared budget. Truncations are kept for reporting only, but the child's
// entries count against the shared entry budget, so exhausted entry shares
// still truncate the later lanes.
func (b *devArtifactScanBudget) mergeTempPathBudget(child *devArtifactScanBudget) {
	if b == nil || child == nil || child == b {
		return
	}
	b.tempRoots += child.tempRoots
	if child.exhausted() {
		b.tempPathsTruncated++
	}
	b.mergeRootBudget(child)
}

func optionalDevArtifactScanBudget(budgets []*devArtifactScanBudget) *devArtifactScanBudget {
	if len(budgets) == 0 {
		return nil
	}
	return budgets[0]
}

func (b *devArtifactScanBudget) context(ctx context.Context) (context.Context, context.CancelFunc) {
	if b == nil || b.maxDuration <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, b.maxDuration)
}

func (b *devArtifactScanBudget) checkPath(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		if b != nil && b.maxDuration > 0 && errors.Is(err, context.DeadlineExceeded) {
			b.markTruncated(path, fmt.Sprintf("scan duration exceeded %s", b.maxDuration))
			return errDevArtifactScanBudgetExceeded
		}
		return err
	}
	if b == nil {
		return nil
	}
	if b.maxEntries > 0 && b.entries >= b.maxEntries {
		b.markTruncated(path, fmt.Sprintf("scan entry budget exceeded %d entries", b.maxEntries))
		return errDevArtifactScanBudgetExceeded
	}
	b.entries++
	return nil
}

func (b *devArtifactScanBudget) checkTempRoot(ctx context.Context, path string) error {
	if err := b.checkPath(ctx, path); err != nil {
		return err
	}
	if b == nil {
		return nil
	}
	cleanPath := filepath.Clean(path)
	if _, ok := b.tempRootSeen[cleanPath]; ok {
		return nil
	}
	if b.tempMaxRoots > 0 && b.tempRoots >= b.tempMaxRoots {
		b.markTruncated(path, fmt.Sprintf("temporary root budget exceeded %d roots", b.tempMaxRoots))
		return errDevArtifactScanBudgetExceeded
	}
	b.tempRootSeen[cleanPath] = struct{}{}
	b.tempRoots++
	return nil
}

// sizeDir sizes an artifact directory and counts the entries the sizing walk
// visits. The sizing walk is reported (scan_sizing_entries_visited) but not
// enforced against the entry budget, because a cut-off size would be wrong
// evidence; the shared and root deadlines still bound it through ctx.
func (b *devArtifactScanBudget) sizeDir(ctx context.Context, path string) (int64, error) {
	var size int64
	visited := 0
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		visited++
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	if b != nil {
		b.sizingEntries += visited
	}
	if err != nil {
		return size, err
	}
	return size, ctx.Err()
}

func (b *devArtifactScanBudget) markContextError(ctx context.Context, path string) {
	if b == nil || b.maxDuration <= 0 || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return
	}
	b.markTruncated(path, fmt.Sprintf("scan duration exceeded %s", b.maxDuration))
}

func (b *devArtifactScanBudget) markTruncated(path, reason string) {
	if b == nil {
		return
	}
	if len(b.truncatedPath) >= maxRecordedTruncations {
		return
	}
	b.truncatedPath[path] = reason
}

func (b *devArtifactScanBudget) markRootTruncated(path, reason string) {
	if b == nil {
		return
	}
	if b.rootTruncatedPath == nil {
		b.rootTruncatedPath = map[string]string{}
	}
	if len(b.rootTruncatedPath) >= maxRecordedTruncations {
		return
	}
	b.rootTruncatedPath[path] = reason
}

// exhausted reports whether this budget itself was exhausted. On the shared
// budget it gates the remaining walk lanes; per-root truncations do not (see
// partial).
func (b *devArtifactScanBudget) exhausted() bool {
	return b != nil && len(b.truncatedPath) > 0
}

// partial reports whether any scan evidence is incomplete, including
// truncations confined to a single root's share.
func (b *devArtifactScanBudget) partial() bool {
	return b != nil && (len(b.truncatedPath) > 0 || len(b.rootTruncatedPath) > 0)
}

func (b *devArtifactScanBudget) truncatedDetails() []string {
	if b == nil || !b.partial() {
		return nil
	}
	details := make([]string, 0, len(b.truncatedPath)+len(b.rootTruncatedPath))
	for path, reason := range b.truncatedPath {
		details = append(details, path+" ("+reason+")")
	}
	for path, reason := range b.rootTruncatedPath {
		details = append(details, path+" ("+reason+")")
	}
	sort.Strings(details)
	return details
}

func (b *devArtifactScanBudget) annotatePlan(plan *CleanupPlan) {
	if b == nil {
		return
	}
	plan.Metadata["scan_max_duration"] = b.maxDuration.String()
	plan.Metadata["scan_max_entries"] = strconv.Itoa(b.maxEntries)
	plan.Metadata["scan_root_share"] = "carry_over"
	plan.Metadata["workspace_scan_max_roots"] = strconv.Itoa(b.workspaceMaxRoots)
	plan.Metadata["workspace_roots_visited"] = strconv.Itoa(b.workspaceRootsVisited)
	plan.Metadata["workspace_roots_skipped"] = strconv.Itoa(b.workspaceRootsSkipped)
	plan.Metadata["workspace_roots_truncated"] = strconv.Itoa(b.workspaceRootsTruncated)
	plan.Metadata["temp_scan_max_roots"] = strconv.Itoa(b.tempMaxRoots)
	plan.Metadata["scan_entries_visited"] = strconv.Itoa(b.entries)
	plan.Metadata["scan_sizing_entries_visited"] = strconv.Itoa(b.sizingEntries)
	plan.Metadata["temp_roots_visited"] = strconv.Itoa(b.tempRoots)
	plan.Metadata["temp_scan_max_roots_scope"] = "per_temp_scan_path"
	plan.Metadata["temp_scan_paths_truncated"] = strconv.Itoa(b.tempPathsTruncated)
	plan.Metadata["scan_budget_shared_exhausted"] = strconv.FormatBool(b.exhausted())
	plan.Metadata["scan_budget_exhausted"] = strconv.FormatBool(b.partial())
	if !b.partial() {
		return
	}
	details := b.truncatedDetails()
	plan.Metadata["scan_truncated_paths"] = strings.Join(details, "; ")
	plan.Warnings = append(plan.Warnings,
		"dev-artifacts scan budget was exhausted; dry-run evidence is partial and omitted paths are not cleanup candidates",
		"dev-artifacts scan truncated at: "+strings.Join(details, "; "),
	)
}
